package helm_resources_manager

import (
	"context"
	"fmt"
	"time"

	log2 "github.com/flant/addon-operator/pkg/log"
	log "github.com/sirupsen/logrus"

	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/flant/shell-operator/pkg/kube"
	"github.com/flant/shell-operator/pkg/utils/manifest"

	"github.com/flant/addon-operator/pkg/utils"
)

const monitorDelay = time.Second * 5

type ResourcesMonitor struct {
	ctx    context.Context
	cancel context.CancelFunc
	paused bool

	moduleName       string
	manifests        []manifest.Manifest
	defaultNamespace string

	kubeClient kube.KubernetesClient
	logLabels  map[string]string

	absentCb func(moduleName string, absent []manifest.Manifest, defaultNs string)
}

func NewResourcesMonitor() *ResourcesMonitor {
	return &ResourcesMonitor{
		paused:    false,
		logLabels: make(map[string]string, 0),
		manifests: make([]manifest.Manifest, 0),
	}
}

func (r *ResourcesMonitor) WithContext(ctx context.Context) {
	r.ctx, r.cancel = context.WithCancel(ctx)
}

func (r *ResourcesMonitor) Stop() {
	if r.cancel != nil {
		r.cancel()
	}
}

func (r *ResourcesMonitor) WithKubeClient(client kube.KubernetesClient) {
	r.kubeClient = client
}

func (r *ResourcesMonitor) WithLogLabels(logLabels map[string]string) {
	r.logLabels = logLabels
}

func (r *ResourcesMonitor) WithModuleName(name string) {
	r.moduleName = name
	r.logLabels["module"] = name
}

func (r *ResourcesMonitor) WithDefaultNamespace(ns string) {
	r.defaultNamespace = ns
}

func (r *ResourcesMonitor) WithManifests(manifests []manifest.Manifest) {
	r.manifests = manifests
}

func (r *ResourcesMonitor) WithAbsentCb(cb func(string, []manifest.Manifest, string)) {
	r.absentCb = cb
}

// Start creates a timer and check if all manifests are present in cluster.
func (r *ResourcesMonitor) Start() {
	logEntry := log.WithFields(utils.LabelsToLogFields(r.logLabels)).
		WithField("operator.component", "HelmResourceMonitor")
	go func() {
		timer := time.NewTicker(monitorDelay)

		for {
			select {
			case <-timer.C:
				if r.paused {
					continue
				}
				// Check resources
				absent, err := r.AbsentResources()
				if err != nil {
					logEntry.Errorf("Cannot list helm resources: %s", err)
				}

				if len(absent) > 0 {
					logEntry.Debug("Absent resources detected")
					if r.absentCb != nil {
						r.absentCb(r.moduleName, absent, r.defaultNamespace)
					}
				} else {
					logEntry.Debug("No absent resources detected")
				}

			case <-r.ctx.Done():
				timer.Stop()
				return
			}
		}
	}()
}

// Pause prevent execution of absent callback
func (r *ResourcesMonitor) Pause() {
	r.paused = true
}

// Resume allows execution of absent callback
func (r *ResourcesMonitor) Resume() {
	r.paused = false
}

func (r *ResourcesMonitor) AbsentResources() ([]manifest.Manifest, error) {
	res := make([]manifest.Manifest, 0)

	for _, m := range r.manifests {
		// Get GVR
		//log.Debugf("%s: discover GVR for apiVersion '%s' kind '%s'...", ei.Monitor.Metadata.DebugName, ei.Monitor.ApiVersion, ei.Monitor.Kind)
		//apiRes, err := r.kubeClient.APIResource(m.ApiVersion(), m.Kind())
		var apiRes v1.APIResource
		var err error
		log2.MeasureTimeToLog(func() {
			apiRes, err = r.kubeClient.APIResource(m.ApiVersion(), m.Kind())
		}, fmt.Sprintf("kubeClient.APIResource apiVer=%s kind=%s", m.ApiVersion(), m.Kind()), nil)
		if err != nil {
			//log.Errorf("%s: Cannot get GroupVersionResource info for apiVersion '%s' kind '%s' from api-server. Possibly CRD is not created before informers are started. Error was: %v", ei.Monitor.Metadata.DebugName, ei.Monitor.ApiVersion, ei.Monitor.Kind, err)
			return nil, err
		}
		//log.Debugf("%s: GVR for kind '%s' is '%s'", ei.Monitor.Metadata.DebugName, ei.Monitor.Kind, ei.GroupVersionResource.String())

		gvr := schema.GroupVersionResource{
			Group:    apiRes.Group,
			Version:  apiRes.Version,
			Resource: apiRes.Name,
		}
		// Resources are filtered by metadata.name field. Object is considered absent if list is empty.
		listOptions := v1.ListOptions{
			FieldSelector: fields.OneTermEqualSelector("metadata.name", m.Name()).String(),
		}

		var objList *unstructured.UnstructuredList

		if apiRes.Namespaced {
			ns := m.Namespace(r.defaultNamespace)
			log2.MeasureTimeToLog(func() {
				objList, err = r.kubeClient.Dynamic().Resource(gvr).Namespace(ns).List(listOptions)
			}, fmt.Sprintf("Dynamic Namespaced List of %s", gvr.String()), nil)
		} else {
			log2.MeasureTimeToLog(func() {
				objList, err = r.kubeClient.Dynamic().Resource(gvr).List(listOptions)
			}, fmt.Sprintf("Dynamic List of %s", gvr.String()), nil)
		}

		if apiRes.Namespaced {
			ns := m.Namespace(r.defaultNamespace)
			log2.MeasureTimeToLog(func() {
				_, err = r.kubeClient.Dynamic().Resource(gvr).Namespace(ns).Get(m.Name(), v1.GetOptions{})
			}, fmt.Sprintf("Dynamic Namespaced Get of %s", gvr.String()), nil)
		} else {
			log2.MeasureTimeToLog(func() {
				_, err = r.kubeClient.Dynamic().Resource(gvr).Get(m.Name(), v1.GetOptions{})
			}, fmt.Sprintf("Dynamic Get of %s", gvr.String()), nil)
		}

		if err != nil {
			return nil, fmt.Errorf("Fetch list for helm resource %s: %s", m.Id(), err)
		}

		if len(objList.Items) == 0 {
			res = append(res, m)
		}
	}

	return res, nil
}
