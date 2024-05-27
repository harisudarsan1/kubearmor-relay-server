package elasticsearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/dustin/go-humanize"
	"github.com/elastic/go-elasticsearch/v7"
	"github.com/elastic/go-elasticsearch/v7/esutil"
	"github.com/google/uuid"
	kg "github.com/kubearmor/kubearmor-relay-server/relay-server/log"
	"github.com/kubearmor/kubearmor-relay-server/relay-server/server"
	"k8s.io/client-go/informers"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"

	"k8s.io/client-go/kubernetes"

	"sync"

	"k8s.io/client-go/rest"
)

var (
	countSuccessful uint64
	countEntered    uint64
	start           time.Time
)

// ElasticsearchClient Structure
type ElasticsearchClient struct {
	kaClient    *server.LogClient
	esClient    *elasticsearch.Client
	cancel      context.CancelFunc
	bulkIndexer esutil.BulkIndexer
	ctx         context.Context
	alertCh     chan interface{}
	logCh       chan interface{}
	client      *Client
}

type PodServiceInfo struct {
	Type           string
	PodName        string
	DeploymentName string
	ServiceName    string
}

type ClusterCache struct {
	mu         *sync.RWMutex
	ipPodCache map[string]PodServiceInfo
}

func (cc *ClusterCache) Get(IP string) (PodServiceInfo, bool) {
	cc.mu.RLock()
	defer cc.mu.Unlock()
	value, ok := cc.ipPodCache[IP]
	return value, ok

}
func (cc *ClusterCache) Set(IP string, pi PodServiceInfo) {
	cc.mu.Lock()
	defer cc.mu.Unlock()

	kg.Printf("Received IP and cached %s", IP)
	cc.ipPodCache[IP] = pi

}
func (cc *ClusterCache) Delete(IP string) {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	delete(cc.ipPodCache, IP)

}

type Client struct {
	k8sClient      *kubernetes.Clientset
	ClusterIPCache *ClusterCache
}

func getK8sClient() *kubernetes.Clientset {
	config, err := rest.InClusterConfig()
	if err != nil {
		kg.Errf("Error creating Kubernetes config: %v\n", err)
		return nil
	}
	kg.Printf("Successfully created k8s config")

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		kg.Errf("Error creating Kubernetes clientset: %v\n", err)
		return nil
	}
	kg.Printf("Successfully created k8sClientSet")
	return clientset
}

// NewElasticsearchClient creates a new Elasticsearch client with the given Elasticsearch URL
// and kubearmor LogClient with endpoint. It has a retry mechanism for certain HTTP status codes and a backoff function for retry delays.
// It then creates a new NewBulkIndexer with the esClient
func NewElasticsearchClient(esURL, Endpoint string) (*ElasticsearchClient, error) {
	retryBackoff := backoff.NewExponentialBackOff()
	cfg := elasticsearch.Config{
		Addresses: []string{esURL},

		// Retry on 429 TooManyRequests statuses
		RetryOnStatus: []int{502, 503, 504, 429},

		// Configure the backoff function
		RetryBackoff: func(i int) time.Duration {
			if i == 1 {
				retryBackoff.Reset()
			}
			return retryBackoff.NextBackOff()
		},
		MaxRetries: 5,
	}

	esClient, err := elasticsearch.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create Elasticsearch client: %v", err)
	}
	bi, err := esutil.NewBulkIndexer(esutil.BulkIndexerConfig{
		Client:        esClient,         // The Elasticsearch client
		FlushBytes:    1000000,          // The flush threshold in bytes [1mb]
		FlushInterval: 30 * time.Second, // The periodic flush interval [30 secs]
	})
	if err != nil {
		log.Fatalf("Error creating the indexer: %s", err)
	}
	alertCh := make(chan interface{}, 10000)
	logCh := make(chan interface{}, 10000)
	kaClient := server.NewClient(Endpoint)

	k8sClient := getK8sClient()
	cc := &ClusterCache{
		mu: &sync.RWMutex{},

		ipPodCache: make(map[string]PodServiceInfo),
	}
	client := &Client{
		k8sClient:      k8sClient,
		ClusterIPCache: cc,
	}

	go startInformers(client)

	return &ElasticsearchClient{kaClient: kaClient, bulkIndexer: bi, esClient: esClient, alertCh: alertCh, logCh: logCh, client: client}, nil
}

func startInformers(client *Client) {

	informerFactory := informers.NewSharedInformerFactory(client.k8sClient, time.Minute*10)
	kg.Printf("informerFactory created")

	podInformer := informerFactory.Core().V1().Pods().Informer()
	kg.Printf("pod informers created")

	// Set up event handlers for Pods
	podInformer.AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {

				pod := obj.(*v1.Pod)
				deploymentName := getDeploymentNamefromPod(pod)
				podInfo := PodServiceInfo{
					Type:           "POD",
					PodName:        pod.Name,
					DeploymentName: deploymentName,
				}

				client.ClusterIPCache.Set(pod.Status.PodIP, podInfo)
				
				kg.Printf("POD Added: %s/%s, remoteIP %s\n", pod.Name, deploymentName,pod.Status.PodIP)
			},
			UpdateFunc: func(oldObj, newObj interface{}) {

				pod := newObj.(*v1.Pod)
				deploymentName := getDeploymentNamefromPod(pod)
				podInfo := PodServiceInfo{

					Type:           "POD",
					PodName:        pod.Name,
					DeploymentName: deploymentName,
				}
				
				client.ClusterIPCache.Set(pod.Status.PodIP, podInfo)
				kg.Printf("POD Updated: %s/%s, remoteIP %s\n", pod.Name, deploymentName,pod.Status.PodIP)
				
			},
			DeleteFunc: func(obj interface{}) {

				pod := obj.(*v1.Pod)

				client.ClusterIPCache.Delete(pod.Status.PodIP)
			},
		},
	)

	// Get the Service informer
	serviceInformer := informerFactory.Core().V1().Services().Informer()

	// Set up event handlers
	serviceInformer.AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				service := obj.(*v1.Service)

				svcInfo := PodServiceInfo{

					Type:           "Service",
					ServiceName:    service.Name,
					DeploymentName: "",
				}
				client.ClusterIPCache.Set(service.Spec.ClusterIP, svcInfo)
				
				kg.Printf("Service Added: %s/%s, remoteIP %s\n", service.Namespace, service.Name,service.Spec.ClusterIP)
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				service := newObj.(*v1.Service)

				svcInfo := PodServiceInfo{

					Type:           "Service",
					ServiceName:    service.Name,
					DeploymentName: "",
				}
				client.ClusterIPCache.Set(service.Spec.ClusterIP, svcInfo)
				kg.Printf("Service Updated: %s/%s\n", service.Namespace, service.Name)
			},
			DeleteFunc: func(obj interface{}) {
				service := obj.(*v1.Service)

				client.ClusterIPCache.Delete(service.Spec.ClusterIP)
				// kg.Printf("Service Deleted: %s/%s\n", service.Namespace, service.Name)
			},
		},
	)

	// Start the informer
	stopCh := make(chan struct{})
	defer close(stopCh)

	informerFactory.Start(stopCh)
	informerFactory.WaitForCacheSync(stopCh)

	// Wait for signals to exit
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	<-sigs
}

func getDeploymentNamefromPod(pod *v1.Pod) string {
	for _, ownerReference := range pod.OwnerReferences {
		if ownerReference.Kind == "ReplicaSet" || ownerReference.Kind == "Deployment" || ownerReference.Kind == "Daemonset" {
			// Get the deployment name from the ReplicaSet name
			return ownerReference.Name
		}
	}

	return "None"
}

// bulkIndex takes an interface and index name and adds the data to the Elasticsearch bulk indexer.
// The bulk indexer flushes after the FlushBytes or FlushInterval thresholds are reached.
// The method generates a UUID as the document ID and includes success and failure callbacks for each item added to the bulk indexer.
func (ecl *ElasticsearchClient) bulkIndex(a interface{}, index string) {
	countEntered++
	data, err := json.Marshal(a)
	if err != nil {
		log.Fatalf("Error marshaling data: %s", err)
	}

	err = ecl.bulkIndexer.Add(
		ecl.ctx,
		esutil.BulkIndexerItem{
			Index:      index,
			Action:     "index",
			DocumentID: uuid.New().String(),
			Body:       bytes.NewReader(data),
			OnSuccess: func(ctx context.Context, item esutil.BulkIndexerItem, res esutil.BulkIndexerResponseItem) {
				atomic.AddUint64(&countSuccessful, 1)
			},
			OnFailure: func(ctx context.Context, item esutil.BulkIndexerItem, res esutil.BulkIndexerResponseItem, err error) {
				if err != nil {
					log.Printf("ERROR: %s", err)
				} else {
					log.Printf("ERROR: %s: %s", res.Error.Type, res.Error.Reason)
				}
			},
		},
	)

	if err != nil {
		log.Fatalf("Error adding items to bulk indexer: %s", err)
	}
}

// Start starts the Elasticsearch client by performing a health check on the gRPC server
// and starting goroutines to consume messages from the alert channel and bulk index them.
// The method starts a goroutine for each stream and waits for messages to be received.
// Additional goroutines consume alert from the alert channel and bulk index them.
func (ecl *ElasticsearchClient) Start() error {
	start = time.Now()
	client := ecl.kaClient
	ecl.ctx, ecl.cancel = context.WithCancel(context.Background())

	// do healthcheck
	if ok := client.DoHealthCheck(); !ok {
		return fmt.Errorf("failed to check the liveness of the gRPC server")
	}
	kg.Printf("Checked the liveness of the gRPC server")

	client.WgServer.Add(1)
	go func() {
		defer client.WgServer.Done()
		for client.Running {
			res, err := client.AlertStream.Recv()
			if err != nil {
				kg.Warnf("Failed to receive an alert (%s)", client.Server)
				break
			}
			tel, _ := json.Marshal(res)
			fmt.Printf("%s\n", string(tel))
			ecl.alertCh <- res
		}
	}()

	for i := 0; i < 5; i++ {
		go func() {
			for {
				select {
				case alert := <-ecl.alertCh:
					ecl.bulkIndex(alert, "alert")
				case <-ecl.ctx.Done():
					close(ecl.alertCh)
					return
				}
			}
		}()
	}

	client.WgServer.Add(1)
	go func() {
		defer client.WgServer.Done()
		for client.Running {
			res, err := client.LogStream.Recv()
			if err != nil {
				kg.Warnf("Failed to receive an log (%s)", client.Server)
				break
			}

			if containsKprobe := strings.Contains(res.Data, "kprobe"); containsKprobe {

				resourceMap := extractdata(res.GetResource())
				remoteIP := resourceMap["remoteip"]
				podserviceInfo, found := ecl.client.ClusterIPCache.Get(remoteIP)
				if found {
					switch podserviceInfo.Type {
					case "POD":
						resource := res.GetResource() + fmt.Sprintf(" Deploymentname:%s", podserviceInfo.DeploymentName)
						data := res.GetData() + fmt.Sprintf(" OwnerType:pod")
						res.Data = data

						res.Resource = resource
						kg.Printf("logData:%s", res.Data)
						break
					case "SERVICE":
						resource := res.GetResource() + fmt.Sprintf(" ServiceName:%s", podserviceInfo.ServiceName)

						data := res.GetData() + fmt.Sprintf(" OwnerType:service")
						res.Data = data
						res.Resource = resource
						kg.Printf("logData:%s", res.Data)

						break
					}
				}

			}
			tel, _ := json.Marshal(res)
			fmt.Printf("%s\n", string(tel))
			ecl.logCh <- res
		}
	}()

	for i := 0; i < 5; i++ {
		go func() {
			for {
				select {
				case log := <-ecl.logCh:
					ecl.bulkIndex(log, "log")
					kg.Print("received log and indexed")
				case <-ecl.ctx.Done():
					close(ecl.logCh)
					return
				}
			}
		}()
	}
	return nil
}

// Stop stops the Elasticsearch client and performs necessary cleanup operations.
// It stops the Kubearmor Relay client, closes the BulkIndexer and cancels the context.
func (ecl *ElasticsearchClient) Stop() error {
	logClient := ecl.kaClient
	logClient.Running = false
	time.Sleep(2 * time.Second)

	//Destoy KubeArmor Relay Client
	if err := logClient.DestroyClient(); err != nil {
		return fmt.Errorf("failed to destroy the kubearmor relay gRPC client (%s)", err.Error())
	}
	kg.Printf("Destroyed kubearmor relay gRPC client")

	//Close BulkIndexer
	if err := ecl.bulkIndexer.Close(ecl.ctx); err != nil {
		kg.Errf("Unexpected error: %s", err)
	}

	ecl.cancel()

	kg.Printf("Stopped kubearmor receiver")
	time.Sleep(2 * time.Second)
	ecl.PrintBulkStats()
	return nil
}

// PrintBulkStats prints data on the bulk indexing process, including the number of indexed documents,
// the number of errors, and the indexing rate , after elasticsearch client stops
func (ecl *ElasticsearchClient) PrintBulkStats() {
	biStats := ecl.bulkIndexer.Stats()
	println(strings.Repeat("▔", 80))

	dur := time.Since(start)

	if biStats.NumFailed > 0 {
		fmt.Printf(
			"Indexed [%s] documents with [%s] errors in %s (%s docs/sec)",
			humanize.Comma(int64(biStats.NumFlushed)),
			humanize.Comma(int64(biStats.NumFailed)),
			dur.Truncate(time.Millisecond),
			humanize.Comma(int64(1000.0/float64(dur/time.Millisecond)*float64(biStats.NumFlushed))),
		)
	} else {
		log.Printf(
			"Sucessfuly indexed [%s] documents in %s (%s docs/sec)",
			humanize.Comma(int64(biStats.NumFlushed)),
			dur.Truncate(time.Millisecond),
			humanize.Comma(int64(1000.0/float64(dur/time.Millisecond)*float64(biStats.NumFlushed))),
		)
	}
	println(strings.Repeat("▔", 80))
}
