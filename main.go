package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/comail/colog"
	"github.com/julienschmidt/httprouter"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/wait"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	schedulerapi "k8s.io/kubernetes/pkg/scheduler/apis/extender/v1"
)

const (
	versionPath      = "/version"
	apiPrefix        = "/scheduler"
	bindPath         = apiPrefix + "/bind"
	preemptionPath   = apiPrefix + "/preemption"
	predicatesPrefix = apiPrefix + "/predicates"
	prioritiesPrefix = apiPrefix + "/priorities"
)

var (
	version string // injected via ldflags at build time

	config, _         = rest.InClusterConfig()
	clientSet         = kubernetes.NewForConfigOrDie(config)
	podListWatcher    = cache.NewListWatchFromClient(clientSet.CoreV1().RESTClient(), "pods", v1.NamespaceAll, fields.Everything())
	indexer, informer = cache.NewIndexerInformer(podListWatcher,
		&v1.Pod{},
		time.Hour*24,
		cache.ResourceEventHandlerFuncs{},
		cache.Indexers{"node": indexByPodNodeName})

	TruePredicate = Predicate{
		Name: "always_true",
		Func: func(pod v1.Pod, node v1.Node) (bool, error) {
			return true, nil
		},
	}

	GroupPriority = Prioritize{
		Name: "group_score",
		Func: func(_ v1.Pod, nodes []v1.Node) (*schedulerapi.HostPriorityList, error) {
			var priorityList schedulerapi.HostPriorityList
			priorityList = make([]schedulerapi.HostPriority, len(nodes))

			for i, node := range nodes {
				priorityList[i] = schedulerapi.HostPriority{
					Host:  node.Name,
					Score: 1000,
				}

				if group, ok := node.Labels["group"]; ok && group == "Scale" {
					// Details: (cpu(10 * sum(requested) / capacity) + memory(10 * sum(requested) / capacity)) / 2
					pods, err := indexer.ByIndex("node", node.Name)
					if err != nil{
						priorityList[i].Score = 0
						log.Println(err)
						continue
					}
					cpu, mem:= &resource.Quantity{}, &resource.Quantity{}
					for _, obj := range pods{
						if pod, ok := obj.(*v1.Pod); ok{
							for _, container := range pod.Spec.Containers{
								cpu.Add(*container.Resources.Requests.Cpu())
								mem.Add(*container.Resources.Requests.Memory())
							}
						}else{
							log.Println("not pod")
						}
					}
					nodeCpu, nodeMem := node.Status.Capacity.Cpu(), node.Status.Capacity.Memory()
					score := (toFloat(cpu)/toFloat(nodeCpu) + toFloat(mem)/toFloat(nodeMem))* 100.0
					priorityList[i].Score = int64(score)
				}
				log.Printf("score for %s %d\n", node.Name, priorityList[i].Score)
			}
			return &priorityList, nil
		},
	}

	NoBind = Bind{
		Func: func(podName string, podNamespace string, podUID types.UID, node string) error {
			return fmt.Errorf("This extender doesn't support Bind.  Please make 'BindVerb' be empty in your ExtenderConfig.")
		},
	}

)

func toFloat(q *resource.Quantity) float64{
	a, _ := q.AsInt64()
	return float64(a)
}

func indexByPodNodeName(obj interface{}) ([]string, error) {
	pod, ok := obj.(*v1.Pod)
	if !ok {
		return []string{}, nil
	}
	// We are only interested in active pods with nodeName set
	if len(pod.Spec.NodeName) == 0 || pod.Status.Phase == v1.PodSucceeded || pod.Status.Phase == v1.PodFailed {
		return []string{}, nil
	}
	return []string{pod.Spec.NodeName}, nil
}

func StringToLevel(levelStr string) colog.Level {
	switch level := strings.ToUpper(levelStr); level {
	case "TRACE":
		return colog.LTrace
	case "DEBUG":
		return colog.LDebug
	case "INFO":
		return colog.LInfo
	case "WARNING":
		return colog.LWarning
	case "ERROR":
		return colog.LError
	case "ALERT":
		return colog.LAlert
	default:
		log.Printf("warning: LOG_LEVEL=\"%s\" is empty or invalid, fallling back to \"INFO\".\n", level)
		return colog.LInfo
	}
}

func main() {
	colog.SetDefaultLevel(colog.LInfo)
	colog.SetMinLevel(colog.LInfo)
	colog.SetFormatter(&colog.StdFormatter{
		Colors: true,
		Flag:   log.Ldate | log.Ltime | log.Lshortfile,
	})
	colog.Register()
	level := StringToLevel(os.Getenv("LOG_LEVEL"))
	log.Print("Log level was set to ", strings.ToUpper(level.String()))
	colog.SetMinLevel(level)

	go informer.Run(wait.NeverStop)
	cache.WaitForCacheSync(wait.NeverStop, informer.HasSynced)

	router := httprouter.New()
	AddVersion(router)

	predicates := []Predicate{TruePredicate}
	for _, p := range predicates {
		AddPredicate(router, p)
	}

	priorities := []Prioritize{GroupPriority}
	for _, p := range priorities {
		AddPrioritize(router, p)
	}

	AddBind(router, NoBind)

	log.Print("info: server starting on the port :80")
	if err := http.ListenAndServe(":80", router); err != nil {
		log.Fatal(err)
	}
}
