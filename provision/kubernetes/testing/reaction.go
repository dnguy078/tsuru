// Copyright 2018 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package testing

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	stdRuntime "runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/errors"
	"github.com/tsuru/tsuru/provision"
	tsuruv1 "github.com/tsuru/tsuru/provision/kubernetes/pkg/apis/tsuru/v1"
	faketsuru "github.com/tsuru/tsuru/provision/kubernetes/pkg/client/clientset/versioned/fake"
	tsuruv1client "github.com/tsuru/tsuru/provision/kubernetes/pkg/client/clientset/versioned/typed/tsuru/v1"
	"github.com/tsuru/tsuru/provision/provisiontest"
	_ "github.com/tsuru/tsuru/storage/mongodb"
	provTypes "github.com/tsuru/tsuru/types/provision"
	check "gopkg.in/check.v1"
	appsv1 "k8s.io/api/apps/v1"
	apiv1 "k8s.io/api/core/v1"
	v1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	fakeapiextensions "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	apiextensionsv1beta1 "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1beta1"
	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/apimachinery/pkg/util/httpstream/spdy"
	informers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"
)

const (
	trueStr = "true"
)

type ClusterInterface interface {
	CoreV1() v1core.CoreV1Interface
	RestConfig() *rest.Config
	AppNamespace(provision.App) (string, error)
	PoolNamespace(string) string
	Namespace() string
	GetCluster() *provTypes.Cluster
}

type KubeMock struct {
	client        *ClientWrapper
	Stream        map[string]StreamResult
	LogHook       func(w io.Writer, r *http.Request)
	DefaultHook   func(w http.ResponseWriter, r *http.Request)
	p             provision.Provisioner
	factory       informers.SharedInformerFactory
	HandleSize    bool
	IgnorePool    bool
	IgnoreAppName bool
}

func NewKubeMock(cluster *ClientWrapper, p provision.Provisioner, factory informers.SharedInformerFactory) *KubeMock {
	stream := make(map[string]StreamResult)
	return &KubeMock{
		client:      cluster,
		Stream:      stream,
		LogHook:     nil,
		DefaultHook: nil,
		p:           p,
		factory:     factory,
	}
}

type ClientWrapper struct {
	*fake.Clientset
	ApiExtensionsClientset *fakeapiextensions.Clientset
	TsuruClientset         *faketsuru.Clientset
	ClusterInterface
}

func (c *ClientWrapper) TsuruV1() tsuruv1client.TsuruV1Interface {
	return c.TsuruClientset.TsuruV1()
}

func (c *ClientWrapper) ApiextensionsV1beta1() apiextensionsv1beta1.ApiextensionsV1beta1Interface {
	return c.ApiExtensionsClientset.ApiextensionsV1beta1()
}

func (c *ClientWrapper) CoreV1() v1core.CoreV1Interface {
	core := c.Clientset.CoreV1()
	return &clientCoreWrapper{core, c.ClusterInterface}
}

type clientCoreWrapper struct {
	v1core.CoreV1Interface
	cluster ClusterInterface
}

func (c *clientCoreWrapper) Pods(namespace string) v1core.PodInterface {
	pods := c.CoreV1Interface.Pods(namespace)
	return &clientPodsWrapper{pods, c.cluster}
}

type clientPodsWrapper struct {
	v1core.PodInterface
	cluster ClusterInterface
}

type StreamResult struct {
	Stdin  string
	Resize string
	Urls   []url.URL
}

func (s *KubeMock) DefaultReactions(c *check.C) (*provisiontest.FakeApp, func(), func()) {
	a := provisiontest.NewFakeApp("myapp", "python", 0)
	err := s.p.Provision(a)
	c.Assert(err, check.IsNil)
	a.Deploys = 1
	podReaction, deployPodReady := s.deployPodReaction(a, c)
	servReaction := s.ServiceWithPortReaction(c, nil)
	rollbackDeployment := s.DeploymentReactions(c)
	s.client.PrependReactor("create", "pods", podReaction)
	s.client.PrependReactor("create", "services", servReaction)
	s.client.TsuruClientset.PrependReactor("create", "apps", s.AppReaction(a, c))
	srv, wg := s.CreateDeployReadyServer(c)
	s.MockfakeNodes(c, srv.URL)
	return a, func() {
			rollbackDeployment()
			deployPodReady.Wait()
			wg.Wait()
		}, func() {
			rollbackDeployment()
			deployPodReady.Wait()
			wg.Wait()
			if srv == nil {
				return
			}
			srv.Close()
			srv = nil
		}
}

func (s *KubeMock) NoAppReactions(c *check.C) (func(), func()) {
	podReaction, podReady := s.buildPodReaction(c)
	servReaction := s.ServiceWithPortReaction(c, nil)
	rollbackDeployment := s.DeploymentReactions(c)
	s.client.PrependReactor("create", "pods", podReaction)
	s.client.PrependReactor("create", "services", servReaction)
	srv, wg := s.CreateDeployReadyServer(c)
	s.MockfakeNodes(c, srv.URL)
	return func() {
			rollbackDeployment()
			podReady.Wait()
			wg.Wait()
		}, func() {
			rollbackDeployment()
			podReady.Wait()
			wg.Wait()
			if srv == nil {
				return
			}
			srv.Close()
			srv = nil
		}
}

func (s *KubeMock) CreateDeployReadyServer(c *check.C) (*httptest.Server, *sync.WaitGroup) {
	mu := sync.Mutex{}
	attachFn := func(w http.ResponseWriter, r *http.Request, cont string) {
		tty := r.FormValue("tty") == trueStr
		stdin := r.FormValue("stdin") == trueStr
		stdout := r.FormValue("stdout") == trueStr
		stderr := r.FormValue("stderr") == trueStr
		expected := 1
		if stdin {
			expected++
		}
		if stdout {
			expected++
		}
		if stderr || tty {
			expected++
		}
		_, err := httpstream.Handshake(r, w, []string{"v4.channel.k8s.io"})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		upgrader := spdy.NewResponseUpgrader()
		type streamAndReply struct {
			s httpstream.Stream
			r <-chan struct{}
		}
		streams := make(chan streamAndReply, expected)
		conn := upgrader.UpgradeResponse(w, r, func(stream httpstream.Stream, replySent <-chan struct{}) error {
			streams <- streamAndReply{s: stream, r: replySent}
			return nil
		})
		if conn == nil {
			return
		}
		defer conn.Close()
		waitStreamReply := func(replySent <-chan struct{}, notify chan<- struct{}) {
			<-replySent
			notify <- struct{}{}
		}
		replyChan := make(chan struct{})
		streamMap := map[string]httpstream.Stream{}
		receivedStreams := 0
		timeout := time.After(5 * time.Second)
	WaitForStreams:
		for {
			select {
			case stream := <-streams:
				streamType := stream.s.Headers().Get(apiv1.StreamType)
				streamMap[streamType] = stream.s
				go waitStreamReply(stream.r, replyChan)
			case <-replyChan:
				receivedStreams++
				if receivedStreams == expected {
					break WaitForStreams
				}
			case <-timeout:
				c.Fatalf("timeout waiting for channels, received %d of %d", receivedStreams, expected)
				return
			}
		}
		if resize := streamMap[apiv1.StreamTypeResize]; resize != nil {
			scanner := bufio.NewScanner(resize)
			if s.HandleSize && scanner.Scan() {
				mu.Lock()
				res := s.Stream[cont]
				res.Resize = scanner.Text()
				s.Stream[cont] = res
				mu.Unlock()
			}
		}
		if stdin := streamMap[apiv1.StreamTypeStdin]; stdin != nil {
			data, _ := ioutil.ReadAll(stdin)
			mu.Lock()
			res := s.Stream[cont]
			res.Stdin = string(data)
			s.Stream[cont] = res
			mu.Unlock()
		}
		if stderr := streamMap[apiv1.StreamTypeStderr]; stderr != nil {
			if s.LogHook == nil {
				stderr.Write([]byte("stderr data"))
			}
		}
		if stdout := streamMap[apiv1.StreamTypeStdout]; stdout != nil {
			if s.LogHook != nil {
				s.LogHook(stdout, r)
				return
			}
			stdout.Write([]byte("stdout data"))
		}
	}
	wg := sync.WaitGroup{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wg.Add(1)
		defer wg.Done()
		cont := r.FormValue("container")
		mu.Lock()
		res := s.Stream[cont]
		res.Urls = append(res.Urls, *r.URL)
		s.Stream[cont] = res
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/attach") || strings.HasSuffix(r.URL.Path, "/exec") {
			attachFn(w, r, cont)
		} else if strings.HasSuffix(r.URL.Path, "/log") {
			if s.LogHook != nil {
				s.LogHook(w, r)
				return
			}
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "my log message")
		} else if s.DefaultHook != nil {
			s.DefaultHook(w, r)
		} else if r.URL.Path == "/api/v1/pods" {
			s.ListPodsHandler(c)(w, r)
		}
	}))
	return srv, &wg
}

func (s *KubeMock) ListPodsHandler(c *check.C, funcs ...func(r *http.Request)) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		c.Assert(r.URL.Path, check.Equals, "/api/v1/pods")
		for _, f := range funcs {
			f(r)
		}
		nlist, err := s.client.CoreV1().Namespaces().List(metav1.ListOptions{})
		c.Assert(err, check.IsNil)
		response := apiv1.PodList{}
		namespaces := []string{}
		if len(nlist.Items) == 0 {
			namespaces = []string{"default"}
		}
		for _, n := range nlist.Items {
			namespaces = append(namespaces, n.GetName())
		}
		for _, n := range namespaces {
			podlist, errList := s.client.CoreV1().Pods(n).List(metav1.ListOptions{LabelSelector: r.Form.Get("labelSelector")})
			c.Assert(errList, check.IsNil)
			response.Items = append(response.Items, podlist.Items...)
		}
		w.Header().Add("Content-type", "application/json")
		err = json.NewEncoder(w).Encode(response)
		c.Assert(err, check.IsNil)
	}
}

func SortNodes(nodes []*apiv1.Node) {
	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].Name < nodes[j].Name
	})
}

func (s *KubeMock) WaitNodeUpdate(c *check.C, fn func()) {
	nodes, err := s.p.(provision.NodeProvisioner).ListNodes(nil)
	c.Assert(err, check.IsNil)
	var rawNodes []*apiv1.Node
	for _, n := range nodes {
		rawNodes = append(rawNodes, n.(interface{ RawNode() *apiv1.Node }).RawNode())
	}
	fn()
	timeout := time.After(5 * time.Second)
	for {
		nodes, err = s.p.(provision.NodeProvisioner).ListNodes(nil)
		c.Assert(err, check.IsNil)
		var rawNodesAfter []*apiv1.Node
		for _, n := range nodes {
			rawNodesAfter = append(rawNodesAfter, n.(interface{ RawNode() *apiv1.Node }).RawNode())
		}
		SortNodes(rawNodes)
		SortNodes(rawNodesAfter)
		if !reflect.DeepEqual(rawNodes, rawNodesAfter) {
			return
		}
		select {
		case <-time.After(100 * time.Millisecond):
		case <-timeout:
			c.Fatal("timeout waiting for node changes")
		}
	}
}

func (s *KubeMock) MockfakeNodes(c *check.C, urls ...string) {
	if len(urls) > 0 {
		s.client.GetCluster().Addresses = urls
		s.client.ClusterInterface.RestConfig().Host = urls[0]
	}
	for i := 1; i <= 2; i++ {
		s.WaitNodeUpdate(c, func() {
			_, err := s.client.CoreV1().Nodes().Create(&apiv1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: fmt.Sprintf("n%d", i),
					Labels: map[string]string{
						"tsuru.io/pool": "test-default",
					},
				},
				Status: apiv1.NodeStatus{
					Addresses: []apiv1.NodeAddress{
						{
							Type:    apiv1.NodeInternalIP,
							Address: fmt.Sprintf("192.168.99.%d", i),
						},
						{
							Type:    apiv1.NodeExternalIP,
							Address: fmt.Sprintf("200.0.0.%d", i),
						},
					},
				},
			})
			c.Assert(err, check.IsNil)
		})
	}
}

func (s *KubeMock) AppReaction(a provision.App, c *check.C) ktesting.ReactionFunc {
	return func(action ktesting.Action) (bool, runtime.Object, error) {
		if !s.IgnoreAppName {
			app := action.(ktesting.CreateAction).GetObject().(*tsuruv1.App)
			c.Assert(app.GetName(), check.Equals, a.GetName())
		}
		return RunReactionsAfter(&s.client.TsuruClientset.Fake, action)
	}
}

func (s *KubeMock) CRDReaction(c *check.C) ktesting.ReactionFunc {
	return func(action ktesting.Action) (bool, runtime.Object, error) {
		crd := action.(ktesting.CreateAction).GetObject().(*v1beta1.CustomResourceDefinition)
		crd.Status.Conditions = []v1beta1.CustomResourceDefinitionCondition{
			{Type: v1beta1.Established, Status: v1beta1.ConditionTrue},
		}
		return RunReactionsAfter(&s.client.ApiExtensionsClientset.Fake, action)
	}
}

func UpdatePodContainerStatus(pod *apiv1.Pod, running bool) {
	for _, cont := range pod.Spec.Containers {
		contStatus := apiv1.ContainerStatus{
			Name:  cont.Name,
			State: apiv1.ContainerState{},
		}
		if running {
			contStatus.State.Running = &apiv1.ContainerStateRunning{}
		} else {
			contStatus.State.Terminated = &apiv1.ContainerStateTerminated{
				ExitCode: 0,
			}
		}
		pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, contStatus)
	}
}

func (s *KubeMock) deployPodReaction(a provision.App, c *check.C) (ktesting.ReactionFunc, *sync.WaitGroup) {
	wg := sync.WaitGroup{}
	return func(action ktesting.Action) (bool, runtime.Object, error) {
		pod := action.(ktesting.CreateAction).GetObject().(*apiv1.Pod)
		defer func() {
			err := s.factory.Core().V1().Pods().Informer().GetStore().Add(pod)
			c.Assert(err, check.IsNil)
		}()
		if !s.IgnorePool {
			c.Assert(pod.Spec.NodeSelector, check.DeepEquals, map[string]string{
				"tsuru.io/pool": a.GetPool(),
			})
		}
		c.Assert(pod.ObjectMeta.Labels, check.NotNil)
		c.Assert(pod.ObjectMeta.Labels["tsuru.io/is-tsuru"], check.Equals, trueStr)
		c.Assert(pod.ObjectMeta.Labels["tsuru.io/app-name"], check.Equals, a.GetName())
		c.Assert(pod.ObjectMeta.Labels["tsuru.io/app-platform"], check.Equals, a.GetPlatform())
		if !s.IgnorePool {
			c.Assert(pod.ObjectMeta.Labels["tsuru.io/app-pool"], check.Equals, a.GetPool())
		}
		c.Assert(pod.ObjectMeta.Labels["tsuru.io/provisioner"], check.Equals, "kubernetes")
		c.Assert(pod.ObjectMeta.Annotations, check.NotNil)
		c.Assert(pod.ObjectMeta.Annotations["tsuru.io/router-type"], check.Equals, "fake")
		c.Assert(pod.ObjectMeta.Annotations["tsuru.io/router-name"], check.Equals, "fake")
		if !strings.HasSuffix(pod.Name, "-deploy") {
			return RunReactionsAfter(&s.client.Fake, action)
		}
		pod.Status.StartTime = &metav1.Time{Time: time.Now()}
		pod.Status.Phase = apiv1.PodSucceeded
		pod.Spec.NodeName = "n1"
		toRegister := false
		for _, cont := range pod.Spec.Containers {
			if strings.Contains(strings.Join(cont.Command, " "), "unit_agent") {
				toRegister = true
			}
		}
		if toRegister {
			UpdatePodContainerStatus(pod, true)
			pod.Status.Phase = apiv1.PodRunning
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := s.p.RegisterUnit(a, pod.Name, map[string]interface{}{
					"processes": map[string]interface{}{
						"web":    "python myapp.py",
						"worker": "python myworker.py",
					},
				})
				c.Assert(err, check.IsNil)
				pod.Status.Phase = apiv1.PodSucceeded
				ns, err := s.client.AppNamespace(a)
				c.Assert(err, check.IsNil)
				UpdatePodContainerStatus(pod, false)
				_, err = s.client.CoreV1().Pods(ns).Update(pod)
				c.Assert(err, check.IsNil)
				err = s.factory.Core().V1().Pods().Informer().GetStore().Update(pod)
				c.Assert(err, check.IsNil)
			}()
		}
		return RunReactionsAfter(&s.client.Fake, action)
	}, &wg
}

func (s *KubeMock) buildPodReaction(c *check.C) (ktesting.ReactionFunc, *sync.WaitGroup) {
	wg := sync.WaitGroup{}
	return func(action ktesting.Action) (bool, runtime.Object, error) {
		pod := action.(ktesting.CreateAction).GetObject().(*apiv1.Pod)
		c.Assert(pod.Spec.Affinity, check.NotNil)
		c.Assert(pod.ObjectMeta.Labels, check.NotNil)
		c.Assert(pod.ObjectMeta.Labels["tsuru.io/is-tsuru"], check.Equals, trueStr)
		c.Assert(pod.ObjectMeta.Labels["tsuru.io/provisioner"], check.Equals, "kubernetes")
		c.Assert(pod.ObjectMeta.Annotations, check.NotNil)
		if !strings.HasSuffix(pod.Name, "-image-build") {
			return RunReactionsAfter(&s.client.Fake, action)
		}
		pod.Status.StartTime = &metav1.Time{Time: time.Now()}
		pod.Status.Phase = apiv1.PodSucceeded
		pod.Spec.NodeName = "n1"
		return RunReactionsAfter(&s.client.Fake, action)
	}, &wg
}

func (s *KubeMock) ServiceWithPortReaction(c *check.C, ports []apiv1.ServicePort) ktesting.ReactionFunc {
	return func(action ktesting.Action) (bool, runtime.Object, error) {
		srv := action.(ktesting.CreateAction).GetObject().(*apiv1.Service)
		defer func() {
			err := s.factory.Core().V1().Services().Informer().GetStore().Add(srv)
			c.Assert(err, check.IsNil)
		}()
		if len(srv.Spec.Ports) > 0 && srv.Spec.Ports[0].NodePort != int32(0) {
			return RunReactionsAfter(&s.client.Fake, action)
		}
		if len(ports) == 0 {
			srv.Spec.Ports = []apiv1.ServicePort{
				{
					NodePort: int32(30000),
				},
			}
		} else {
			srv.Spec.Ports = ports
		}
		return RunReactionsAfter(&s.client.Fake, action)
	}
}

func (s *KubeMock) DeploymentReactions(c *check.C) func() {
	depReaction, depPodReady := s.deploymentWithPodReaction(c)
	s.client.PrependReactor("create", "deployments", depReaction)
	s.client.PrependReactor("update", "deployments", depReaction)
	return func() {
		depPodReady.Wait()
	}
}

func (s *KubeMock) deploymentWithPodReaction(c *check.C) (ktesting.ReactionFunc, *sync.WaitGroup) {
	wg := sync.WaitGroup{}
	var counter int32
	return func(action ktesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "" {
			return RunReactionsAfter(&s.client.Fake, action)
		}
		wg.Add(1)
		dep := action.(ktesting.CreateAction).GetObject().(*appsv1.Deployment)
		var specReplicas int32
		if dep.Spec.Replicas != nil {
			specReplicas = *dep.Spec.Replicas
		}
		dep.Status.UpdatedReplicas = specReplicas
		dep.Status.Replicas = specReplicas
		go func() {
			defer wg.Done()
			pod := &apiv1.Pod{
				ObjectMeta: dep.Spec.Template.ObjectMeta,
				Spec:       dep.Spec.Template.Spec,
			}
			pod.Status.Phase = apiv1.PodRunning
			pod.Status.StartTime = &metav1.Time{Time: time.Now()}
			pod.ObjectMeta.Namespace = dep.Namespace
			pod.Spec.NodeName = "n1"
			err := cleanupPods(s.client.ClusterInterface, metav1.ListOptions{
				LabelSelector: labels.SelectorFromSet(labels.Set(dep.Spec.Selector.MatchLabels)).String(),
			}, dep.Namespace, s.factory)
			c.Assert(err, check.IsNil)
			for i := int32(1); i <= specReplicas; i++ {
				id := atomic.AddInt32(&counter, 1)
				pod.ObjectMeta.Name = fmt.Sprintf("%s-pod-%d-%d", dep.Name, id, i)
				_, err = s.client.CoreV1().Pods(dep.Namespace).Create(pod)
				c.Assert(err, check.IsNil)
				err = s.factory.Core().V1().Pods().Informer().GetStore().Add(pod)
				c.Assert(err, check.IsNil)
			}
		}()
		return RunReactionsAfter(&s.client.Fake, action)
	}, &wg
}

func cleanupPods(client ClusterInterface, opts metav1.ListOptions, namespace string, factory informers.SharedInformerFactory) error {
	pods, err := client.CoreV1().Pods(namespace).List(opts)
	if err != nil {
		return errors.WithStack(err)
	}
	for _, pod := range pods.Items {
		err = client.CoreV1().Pods(namespace).Delete(pod.Name, &metav1.DeleteOptions{})
		if err != nil && !k8sErrors.IsNotFound(err) {
			return errors.WithStack(err)
		}
		err = factory.Core().V1().Pods().Informer().GetStore().Delete(&pod)
		if err != nil && !k8sErrors.IsNotFound(err) {
			return errors.WithStack(err)
		}
	}
	return nil
}

// RunReactionsAfter is a hack and it MUST be called from inside a reaction
// function if it modifies the source object and returns false. This code only
// exists because of the behavior change introduced by DeepCopying objects in
// https://github.com/kubernetes/kubernetes/pull/60709.
//
// The regression was identified and should be fixed in
// https://github.com/kubernetes/kubernetes/pull/73601
//
// When the latter PR is merged and we update k8s.io/client-go accordingly this
// code can be safely removed.
func RunReactionsAfter(fake *ktesting.Fake, action ktesting.Action) (bool, runtime.Object, error) {
	running := false
	var pcs [1]uintptr
	// 0 is the Callers call, 1 is the RunReactionsAfter, 2 is the actual caller we want
	stdRuntime.Callers(2, pcs[:])
	frames := stdRuntime.CallersFrames(pcs[:])
	frame, _ := frames.Next()
	for _, reactor := range fake.ReactionChain {
		simpleReactor, ok := reactor.(*ktesting.SimpleReactor)
		if ok && reflect.ValueOf(simpleReactor.Reaction).Pointer() == frame.Entry {
			running = true
			continue
		}
		if !running {
			continue
		}
		if !reactor.Handles(action) {
			continue
		}
		handled, ret, err := reactor.React(action)
		if handled {
			return handled, ret, err
		}
	}
	return false, nil, nil
}
