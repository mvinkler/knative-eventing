/*
Copyright 2019 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package lib

import (
	"bytes"
	"context"
	"fmt"
	"github.com/pkg/errors"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/storage/names"

	pkgTest "knative.dev/pkg/test"
	"knative.dev/pkg/test/helpers"
	"knative.dev/pkg/test/prow"

	// Mysteriously required to support GCP auth (required by k8s libs).
	// Apparently just importing it is enough. @_@ side effects @_@.
	// https://github.com/kubernetes/client-go/issues/242
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	_ "k8s.io/client-go/plugin/pkg/client/auth/oidc"
)

const (
	podLogsDir         = "pod-logs"
	testPullSecretName = "kn-eventing-test-pull-secret"
)

// ComponentsTestRunner is used to run tests against different eventing components.
type ComponentsTestRunner struct {
	ComponentFeatureMap map[metav1.TypeMeta][]Feature
	ComponentsToTest    []metav1.TypeMeta
	componentOptions    map[metav1.TypeMeta][]SetupClientOption
	ComponentName       string
	ComponentNamespace  string
}

// RunTests will use all components that support the given feature, to run
// a test for the testFunc.
func (tr *ComponentsTestRunner) RunTests(
	t *testing.T,
	feature Feature,
	testFunc func(st *testing.T, component metav1.TypeMeta),
) {
	t.Parallel()
	for _, component := range tr.ComponentsToTest {
		// If a component is not present in the map, then assume it has all properties. This is so an
		// unknown component (e.g. a Channel) can be specified via a dedicated flag (e.g. --channels) and have tests run.
		// TODO Use a flag to specify the features of the flag based component, rather than assuming
		// it supports all features.
		features, present := tr.ComponentFeatureMap[component]
		if !present || contains(features, feature) {
			t.Run(fmt.Sprintf("%s-%s", component.Kind, component.APIVersion), func(st *testing.T) {
				testFunc(st, component)
			})
		}
	}
}

// RunTestsWithComponentOptions will use all components that support the given
// feature, to run a test for the testFunc while passing the component specific
// SetupClientOptions to testFunc. You should used this method instead of
// RunTests if you have used AddComponentSetupClientOption to add some component
// specific initialization code. If strict is set to true, tests will not run
// for components that don't exist in the ComponentFeatureMap.
func (tr *ComponentsTestRunner) RunTestsWithComponentOptions(
	t *testing.T,
	feature Feature,
	strict bool,
	testFunc func(st *testing.T, component metav1.TypeMeta,
		options ...SetupClientOption),
) {
	t.Parallel()
	for _, c := range tr.ComponentsToTest {
		component := c
		features, present := tr.ComponentFeatureMap[component]
		subTestName := fmt.Sprintf("%s-%s", component.Kind, component.APIVersion)
		t.Run(subTestName, func(st *testing.T) {
			// If in strict mode and a component is not present in the map, then
			// don't run the tests
			if !strict || (present && contains(features, feature)) {
				testFunc(st, component, tr.componentOptions[component]...)
			} else {
				st.Skipf("Skipping component %s since it did not "+
					"match the feature %s and we are in strict mode", subTestName, feature)
			}
		})
	}
}

// AddComponentSetupClientOption adds a SetupClientOption that should only run when
// component gets selected to run. This should be used when there's an expensive
// initialization code should take place conditionally (e.g. create an instance
// of a source or a channel) as opposed to other cheap initialization code that
// is safe to be called in all cases (e.g. installation of a CRD)
func (tr *ComponentsTestRunner) AddComponentSetupClientOption(component metav1.TypeMeta,
	options ...SetupClientOption) {
	if tr.componentOptions == nil {
		tr.componentOptions = make(map[metav1.TypeMeta][]SetupClientOption)
	}
	if _, ok := tr.componentOptions[component]; !ok {
		tr.componentOptions[component] = make([]SetupClientOption, 0)
	}
	tr.componentOptions[component] = append(tr.componentOptions[component], options...)
}

func contains(features []Feature, feature Feature) bool {
	for _, f := range features {
		if f == feature {
			return true
		}
	}
	return false
}

// SetupClientOption does further setup for the Client. It can be used if other projects
// need to do extra setups to run the tests we expose as test helpers.
type SetupClientOption func(*Client)

// SetupClientOptionNoop is a SetupClientOption that does nothing.
var SetupClientOptionNoop SetupClientOption = func(*Client) {
	// nothing
}

// Setup creates the client objects needed in the e2e tests,
// and does other setups, like creating namespaces, set the test case to run in parallel, etc.
func Setup(t *testing.T, runInParallel bool, options ...SetupClientOption) *Client {
	// Create a new namespace to run this test case.
	namespace := makeK8sNamespace(t.Name())
	t.Logf("namespace is : %q", namespace)
	client, err := NewClient(
		pkgTest.Flags.Kubeconfig,
		pkgTest.Flags.Cluster,
		namespace,
		t)
	if err != nil {
		t.Fatal("Couldn't initialize clients:", err)
	}

	CreateNamespaceIfNeeded(t, client, namespace)

	// Run the test case in parallel if needed.
	if runInParallel {
		t.Parallel()
	}

	// Clean up resources if the test is interrupted in the middle.
	pkgTest.CleanupOnInterrupt(func() { TearDown(client) }, t.Logf)

	// Run further setups for the client.
	for _, option := range options {
		option(client)
	}

	return client
}

func makeK8sNamespace(baseFuncName string) string {
	base := helpers.MakeK8sNamePrefix(baseFuncName)
	return names.SimpleNameGenerator.GenerateName(base + "-")
}

// TearDown will delete created names using clients.
func TearDown(client *Client) {
	if err := client.runCleanup(); err != nil {
		client.T.Logf("Cleanup error: %+v", err)
	}

	// Dump the events in the namespace
	el, err := client.Kube.CoreV1().Events(client.Namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		client.T.Logf("Could not list events in the namespace %q: %v", client.Namespace, err)
	} else {
		// Elements has to be ordered first
		items := el.Items
		sort.SliceStable(items, func(i, j int) bool {
			// Some events might not contain last timestamp, in that case we fallback to event time
			iTime := items[i].LastTimestamp.Time
			if iTime.IsZero() {
				iTime = items[i].EventTime.Time
			}

			jTime := items[j].LastTimestamp.Time
			if jTime.IsZero() {
				jTime = items[j].EventTime.Time
			}

			return iTime.Before(jTime)
		})

		for _, e := range items {
			client.T.Log(formatEvent(&e))
		}
	}

	// If the test is run by CI, export the pod logs in the namespace to the artifacts directory,
	// which will then be uploaded to GCS after the test job finishes.
	if prow.IsCI() {
		dir := filepath.Join(prow.GetLocalArtifactsDir(), podLogsDir)
		client.T.Logf("Export logs in %q to %q", client.Namespace, dir)
		if err := client.ExportLogs(dir); err != nil {
			client.T.Logf("Error in exporting logs: %v", err)
		}
	}

	client.Tracker.Clean(true)
	if err := DeleteNameSpace(client); err != nil {
		client.T.Logf("Could not delete the namespace %q: %v", client.Namespace, err)
	}
}

// Oc type
type Oc struct {
	namespace string
}

// Run the 'oc' CLI with args
func (o Oc) Run(args ...string) (string, error) {
	return RunOc(o.namespace, args...)
}

// RunOc runs "oc" in a given namespace
func RunOc(namespace string, args ...string) (string, error) {
	if namespace != "" {
		args = append(args, "--namespace", namespace)
	}
	stdout, stderr, err := runCli("oc", args)
	if err != nil {
		return stdout, errors.Wrap(err, fmt.Sprintf("stderr: %s", stderr))
	}
	return stdout, nil
}

func runCli(cli string, args []string) (string, string, error) {
	var stderr bytes.Buffer
	var stdout bytes.Buffer

	cmd := exec.Command(cli, args...)
	cmd.Stderr = &stderr
	cmd.Stdout = &stdout
	cmd.Stdin = nil

	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func formatEvent(e *corev1.Event) string {
	return strings.Join([]string{`Event{`,
		`ObjectMeta:` + strings.Replace(strings.Replace(e.ObjectMeta.String(), "ObjectMeta", "v1.ObjectMeta", 1), `&`, ``, 1),
		`InvolvedObject:` + strings.Replace(strings.Replace(e.InvolvedObject.String(), "ObjectReference", "ObjectReference", 1), `&`, ``, 1),
		`Reason:` + e.Reason,
		`Message:` + e.Message,
		`Source:` + strings.Replace(strings.Replace(e.Source.String(), "EventSource", "EventSource", 1), `&`, ``, 1),
		`FirstTimestamp:` + e.FirstTimestamp.String(),
		`LastTimestamp:` + e.LastTimestamp.String(),
		`Count:` + fmt.Sprintf("%d", e.Count),
		`Type:` + e.Type,
		`EventTime:` + e.EventTime.String(),
		`Series:` + strings.Replace(e.Series.String(), "EventSeries", "EventSeries", 1),
		`Action:` + e.Action,
		`Related:` + strings.Replace(e.Related.String(), "ObjectReference", "ObjectReference", 1),
		`ReportingController:` + e.ReportingController,
		`ReportingInstance:` + e.ReportingInstance,
		`}`,
	}, "\n")
}

// CreateNamespaceIfNeeded creates a new namespace if it does not exist.
func CreateNamespaceIfNeeded(t *testing.T, client *Client, namespace string) {

	var (
		errOc     error
		outOc     string
	)

	_, err := client.Kube.CoreV1().Namespaces().Get(context.Background(), namespace, metav1.GetOptions{})

	if err != nil && apierrs.IsNotFound(err) {
		nsSpec := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		_, err = client.Kube.CoreV1().Namespaces().Create(context.Background(), nsSpec, metav1.CreateOptions{})

		if err != nil {
			t.Logf("Failed to create Namespace: %s; %v", namespace, err)
			t.Logf("Using oc new-project instead")

			outOc, errOc = Oc{}.Run("new-project", namespace)
			if errOc == nil {
				t.Logf("stdout: %s", outOc)
			} else {
				t.Fatalf("Failed to create new project: %s; %v", namespace, err)
			}
		}

		// https://github.com/kubernetes/kubernetes/issues/66689
		// We can only start creating pods after the default ServiceAccount is created by the kube-controller-manager.
		err = waitForServiceAccountExists(client, "default", namespace)
		if err != nil {
			t.Fatal("The default ServiceAccount was not created for the Namespace:", namespace)
		}

		// If the "default" Namespace has a secret called
		// "kn-eventing-test-pull-secret" then use that as the ImagePullSecret
		// on the "default" ServiceAccount in this new Namespace.
		// This is needed for cases where the images are in a private registry.
		//_, err := utils.CopySecret(client.Kube.CoreV1(), "default", testPullSecretName, namespace, "default")
		//if err != nil && !apierrs.IsNotFound(err) {
		//	t.Fatalf("error copying the secret into ns %q: %s", namespace, err)
		//}
	}
}

// waitForServiceAccountExists waits until the ServiceAccount exists.
func waitForServiceAccountExists(client *Client, name, namespace string) error {
	return wait.PollImmediate(1*time.Second, 2*time.Minute, func() (bool, error) {
		sas := client.Kube.CoreV1().ServiceAccounts(namespace)
		if _, err := sas.Get(context.Background(), name, metav1.GetOptions{}); err == nil {
			return true, nil
		}
		return false, nil
	})
}

// DeleteNameSpace deletes the namespace that has the given name.
func DeleteNameSpace(client *Client) error {

	client.T.Logf("Deleting Namespace: %s", client.Namespace)

	var (
		errOc     error
		outOc     string
	)

	_, err := client.Kube.CoreV1().Namespaces().Get(context.Background(), client.Namespace, metav1.GetOptions{})
	if err == nil || !apierrs.IsNotFound(err) {
		//return client.Kube.CoreV1().Namespaces().Delete(context.Background(), client.Namespace, metav1.DeleteOptions{})
		errKube := client.Kube.CoreV1().Namespaces().Delete(context.Background(), client.Namespace, metav1.DeleteOptions{})

		if errKube != nil {
			client.T.Logf("Failed to delete Namespace: %s; %v", client.Namespace, errKube)
			client.T.Logf("Using oc delete project instead")

			outOc, errOc = Oc{}.Run("delete", "project", client.Namespace)
			if errOc == nil {
				client.T.Logf("stdout: %s", outOc)
			} else {
				client.T.Logf("TODO: Failed to delete project: %s; %v", client.Namespace, errOc)
			}
			return errOc
		}
		return errKube
	}
	return err
}
