// Package kubernetes provides a kubernetes registry
package kubernetes

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeClient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/micro/go-plugins/registry/kubernetes/client"

	"github.com/micro/go-micro/cmd"
	"github.com/micro/go-micro/registry"

	"github.com/prometheus/client_golang/prometheus"

	logrus "github.com/sirupsen/logrus"
)

type kregistry struct {
	client  client.Kubernetes
	timeout time.Duration
	// test dictionary for cached services
	serviceList map[string][]*registry.Service
	// test dictionary for expire at data
	expireAt  map[string]time.Time
	k8sClient kubeClient.Clientset
}

var (
	log = logrus.WithFields(logrus.Fields{
		"component": "kuber-plugin",
	})

	logProfile = log.WithFields(logrus.Fields{
		"type": "profiling",
	})

	// used on pods as labels & services to select
	// eg: svcSelectorPrefix+"svc.name"
	svcSelectorPrefix = "micro.mu/selector-"
	svcSelectorValue  = "service"

	labelTypeKey          = "micro.mu/type"
	labelTypeValueService = "service"

	// used on k8s services to scope a serialised
	// micro service by pod name
	annotationServiceKeyPrefix = "micro.mu/service-"

	// Pod status
	podRunning = "Running"

	// label name regex
	labelRe = regexp.MustCompilePOSIX("[-A-Za-z0-9_.]")

	// Cache hit and miss counters
	cacheHit     int
	cacheMiss    int
	cacheExpired int

	cache = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kuber_plugin_cache_counter",
		Help: "Go-Micro Kuber-Plugin Cache Counter",
	}, []string{"type"})

	funcDurationsHistogram = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "kuber_plugin_function_durations_histogram_seconds",
		Help:    "Function latency distributions.",
		Buckets: prometheus.DefBuckets,
	}, []string{"function", "cached"})
)

// podSelector
var podSelector = map[string]string{
	labelTypeKey: labelTypeValueService,
}

func init() {
	cmd.DefaultRegistries["kubernetes"] = NewRegistry
	prometheus.Register(cache)
	prometheus.Register(funcDurationsHistogram)
	logrus.SetLevel(logrus.WarnLevel)
}

// serviceName generates a valid service name for k8s labels
func serviceName(name string) string {
	aname := make([]byte, len(name))

	for i, r := range []byte(name) {
		if !labelRe.Match([]byte{r}) {
			aname[i] = '_'
			continue
		}
		aname[i] = r
	}

	return string(aname)
}

// Register sets a service selector label and an annotation with a
// serialised version of the service passed in.
func (c *kregistry) Register(s *registry.Service, opts ...registry.RegisterOption) error {
	var (
		startTime = time.Now()
	)

	if len(s.Nodes) == 0 {
		return errors.New("you must register at least one node")
	}

	// TODO: grab podname from somewhere better than this.
	podName := os.Getenv("HOSTNAME")
	svcName := s.Name

	// encode micro service
	b, err := json.Marshal(s)
	if err != nil {
		fmt.Printf("unable to encode service: %v\n", err)
		return err
	}
	svc := string(b)

	pod := &client.Pod{
		Metadata: &client.Meta{
			Labels: map[string]*string{
				labelTypeKey:                             &labelTypeValueService,
				svcSelectorPrefix + serviceName(svcName): &svcSelectorValue,
			},
			Annotations: map[string]*string{
				annotationServiceKeyPrefix + serviceName(svcName): &svc,
			},
		},
	}

	if _, err := c.client.UpdatePod(podName, pod); err != nil {
		fmt.Printf("unable to update pod: %v\n", err)
		return err
	}

	d := time.Since(startTime)

	funcDurationsHistogram.WithLabelValues("Register", "").Observe(d.Seconds())

	logProfile.Infoln("Register took", d/time.Millisecond, "ms")
	return nil

}

// Deregister nils out any things set in Register
func (c *kregistry) Deregister(s *registry.Service) error {
	return nil

}

// GetService will get all the pods with the given service selector,
// and build services from the annotations.
func (c *kregistry) GetService(name string) ([]*registry.Service, error) {

	var startTime = time.Now()

	var cached = "true"

	// before continue, check if service is cached
	service, err := c.cachedService(name)
	if err != nil {
		return nil, err
	}

	if service == nil {
		// service isn't cached, we need to build it and update the cache
		service, err = c.buildService(name)
		if err != nil {
			return nil, err
		}

		c.updateCachedServices(name, service)

		cached = "false"
	}

	d := time.Since(startTime)

	funcDurationsHistogram.WithLabelValues("GetService", cached).Observe(d.Seconds())

	logProfile.Infoln("GetService took", d/time.Millisecond, "ms")

	return service, nil

}

func (c *kregistry) buildService(name string) ([]*registry.Service, error) {

	startTime := time.Now()

	k8sServices, err := c.k8sClient.CoreV1().Services(metav1.NamespaceAll).List(metav1.ListOptions{})
	if err != nil {
		fmt.Printf("Could not get k8sServices: %v\n", err)
		return nil, err
	}

	var k8sService *v1.Service

	for _, k8ssvc := range k8sServices.Items {
		if k8ssvc.Name == name {
			k8sService = &k8ssvc
			break
		}
	}

	if k8sService == nil {
		fmt.Printf("Could not get k8sServices")
		return nil, fmt.Errorf("Could not find k8s service for %s", name)
	}

	pods, err := c.client.ListPods(map[string]string{
		svcSelectorPrefix + serviceName(name): svcSelectorValue,
	})
	if err != nil {
		fmt.Printf("List of the pods error: %v\n", err)
		return nil, err
	}

	if len(pods.Items) == 0 {
		fmt.Printf("Pod is not found\n")
		return nil, registry.ErrNotFound
	}

	// svcs mapped by version
	svcs := make(map[string]*registry.Service)

	// loop through items
	for _, pod := range pods.Items {
		if pod.Status.Phase != podRunning {
			continue
		}
		// get serialised service from annotation
		svcStr, ok := pod.Metadata.Annotations[annotationServiceKeyPrefix+serviceName(name)]
		if !ok {
			continue
		}

		// unmarshal service string
		var svc registry.Service
		err := json.Unmarshal([]byte(*svcStr), &svc)
		if err != nil {
			return nil, fmt.Errorf("could not unmarshal service '%s' from pod annotation", name)
		}

		// merge up pod service & ip with versioned service.
		vs, ok := svcs[svc.Version]
		if !ok {
			svcs[svc.Version] = &svc
			continue
		}

		vs.Nodes = append(vs.Nodes, svc.Nodes...)
	}

	var list []*registry.Service
	for _, val := range svcs {
		list = append(list, val)
	}

	for _, svc := range list {
		for _, node := range svc.Nodes {
			node.Address = k8sService.Spec.ClusterIP
		}
	}

	d := time.Since(startTime)

	funcDurationsHistogram.WithLabelValues("buildService", "false").Observe(d.Seconds())

	logProfile.Infoln("buildService took", d/time.Millisecond, "ms")

	return list, nil

}

// ListServices will list all the service names
func (c *kregistry) ListServices() ([]*registry.Service, error) {

	startTime := time.Now()

	pods, err := c.client.ListPods(podSelector)
	if err != nil {
		fmt.Printf("Unable to get list of the pods: %v\n", err)
		return nil, err
	}

	// svcs mapped by name
	svcs := make(map[string]bool)

	for _, pod := range pods.Items {
		if pod.Status.Phase != podRunning {
			continue
		}
		for k, v := range pod.Metadata.Annotations {
			if !strings.HasPrefix(k, annotationServiceKeyPrefix) {
				continue
			}

			// we have to unmarshal the annotation itself since the
			// key is encoded to match the regex restriction.
			var svc registry.Service
			if err := json.Unmarshal([]byte(*v), &svc); err != nil {
				continue
			}
			svcs[svc.Name] = true
		}
	}

	var list []*registry.Service
	for val := range svcs {
		list = append(list, &registry.Service{Name: val})
	}

	d := time.Since(startTime)

	funcDurationsHistogram.WithLabelValues("ListServices", "false").Observe(d.Seconds())

	logProfile.Infoln("ListServices took", d/time.Millisecond, "ms")

	return list, nil
}

// Watch returns a kubernetes watcher
func (c *kregistry) Watch() (registry.Watcher, error) {
	return newWatcher(c)
}

func (c *kregistry) String() string {
	return "kubernetes"
}

// NewRegistry creates a kubernetes registry
func NewRegistry(opts ...registry.Option) registry.Registry {

	startTime := time.Now()

	var options registry.Options
	for _, o := range opts {
		o(&options)
	}

	// get first host
	var host string
	if len(options.Addrs) > 0 && "automatic" != options.Addrs[0] {
		host = options.Addrs[0]
	}

	if options.Timeout == 0 {
		options.Timeout = time.Second * 1
	}

	// if no hosts setup, assume InCluster
	var c client.Kubernetes
	if len(host) == 0 {
		c = client.NewClientInCluster()
	} else {
		c = client.NewClientByHost(host)
	}

	// Set up kubernetes client

	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err.Error())
	}

	kubeclient, err := kubeClient.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	d := time.Since(startTime)

	funcDurationsHistogram.WithLabelValues("NewRegistry", "").Observe(d.Seconds())

	logProfile.Infoln("NewRegistry took", d/time.Millisecond, "ms")

	return &kregistry{
		client:      c,
		timeout:     options.Timeout,
		expireAt:    make(map[string]time.Time),
		serviceList: make(map[string][]*registry.Service),
		k8sClient:   *kubeclient,
	}
}

// Test methods for caching
// It using a just native golang dict
func (c *kregistry) cachedService(name string) ([]*registry.Service, error) {

	startTime := time.Now()

	expiredTime, ok := c.expireAt[name]
	// check if exists
	if !ok {
		// cache miss
		cacheMiss++
		cache.WithLabelValues("miss").Inc()
		d := time.Since(startTime)
		funcDurationsHistogram.WithLabelValues("cachedService", "false").Observe(d.Seconds())
		return nil, nil
	}

	var cached string

	// cache has expired, rebuild
	if time.Now().UTC().After(expiredTime.UTC()) {
		cacheExpired++
		cache.WithLabelValues("expired").Inc()
		logProfile.Debugf("Cache Expired. Current time: %v  Expire Time: %v", time.Now().UTC(), expiredTime.UTC())
		service, err := c.buildService(name)
		if err != nil {
			return nil, err
		}
		c.updateCachedServices(name, service)
		cached = "false"
	} else {
		// cache hit
		cacheHit++
		cache.WithLabelValues("hit").Inc()
		cached = "true"
	}

	d := time.Since(startTime)

	funcDurationsHistogram.WithLabelValues("cachedService", cached).Observe(d.Seconds())

	logProfile.Debugf("Cache Hits: %d  Cache Misses: %d  Cache Expiry: %d  Cache Hit vs Miss: %f%%", cacheHit, cacheMiss, cacheExpired, (float32(cacheHit) / float32(cacheHit+cacheExpired+cacheMiss)))
	return c.serviceList[name], nil

}

// updateCachedServices provides setting a service name with a list of registry.Service
// and setting expire time for 20 minutes
func (c *kregistry) updateCachedServices(name string, service []*registry.Service) {
	log.Debugf("Update cache for the service: %s", name)
	c.serviceList[name] = service
	c.expireAt[name] = time.Now().UTC().Add(20 * time.Minute)
	log.Debugf("Cache for the service: %s is updated", name)
}
