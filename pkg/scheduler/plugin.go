package scheduler

import (
	"fmt"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/kubernetes/pkg/api"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/api/meta"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/client"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/client/cache"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/kubelet/envvars"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/labels"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/runtime"
	algorithm "github.com/GoogleCloudPlatform/kubernetes/pkg/scheduler"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/util"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/watch"
	plugin "github.com/GoogleCloudPlatform/kubernetes/plugin/pkg/scheduler"
	log "github.com/golang/glog"
	"github.com/mesos/mesos-go/mesos"
	"github.com/mesosphere/kubernetes-mesos/pkg/queue"
	"gopkg.in/v2/yaml"
)

const (
	enqueuePopTimeout  = 200 * time.Millisecond
	enqueueWaitTimeout = 3 * time.Second
	yieldPopTimeout    = 200 * time.Millisecond
	yieldWaitTimeout   = 3 * time.Second
)

// scheduler abstraction to allow for easier unit testing
type SchedulerInterface interface {
	sync.Locker
	RLocker() sync.Locker
	SlaveIndex

	algorithm() PodScheduleFunc
	createPodTask(api.Context, *api.Pod) (*PodTask, error)
	getTask(taskId string) (task *PodTask, currentState stateType)
	offers() OfferRegistry
	registerPodTask(*PodTask, error) (*PodTask, error)
	taskForPod(podID string) (taskID string, ok bool)
	unregisterPodTask(*PodTask)
	killTask(taskId string) error
	launchTask(*PodTask) error
}

type k8smScheduler struct {
	*KubernetesScheduler
}

func (k *k8smScheduler) algorithm() PodScheduleFunc {
	return k.KubernetesScheduler.scheduleFunc
}

func (k *k8smScheduler) offers() OfferRegistry {
	return k.KubernetesScheduler.offers
}

func (k *k8smScheduler) taskForPod(podID string) (taskID string, ok bool) {
	// assume caller is holding scheduler lock
	taskID, ok = k.podToTask[podID]
	return
}

func (k *k8smScheduler) createPodTask(ctx api.Context, pod *api.Pod) (*PodTask, error) {
	return newPodTask(ctx, pod, k.executor)
}

func (k *k8smScheduler) registerPodTask(task *PodTask, err error) (*PodTask, error) {
	if err == nil {
		// assume caller is holding scheduler lock
		k.podToTask[task.podKey] = task.ID
		k.pendingTasks[task.ID] = task
	}
	return task, err
}

func (k *k8smScheduler) slaveFor(id string) (slave *Slave, ok bool) {
	slave, ok = k.slaves[id]
	return
}

func (k *k8smScheduler) unregisterPodTask(task *PodTask) {
	// assume caller is holding scheduler lock
	delete(k.podToTask, task.podKey)
	delete(k.pendingTasks, task.ID)
}

func (k *k8smScheduler) killTask(taskId string) error {
	// assume caller is holding scheduler lock
	killTaskId := newTaskID(taskId)
	return k.KubernetesScheduler.driver.KillTask(killTaskId)
}

func (k *k8smScheduler) launchTask(task *PodTask) error {
	// assume caller is holding scheduler lock
	taskList := []*mesos.TaskInfo{task.TaskInfo}
	return k.KubernetesScheduler.driver.LaunchTasks(task.Offer.Details().Id, taskList, nil)
}

type binder struct {
	api    SchedulerInterface
	client *client.Client
}

// implements binding.Registry, launches the pod-associated-task in mesos
func (b *binder) Bind(binding *api.Binding) error {

	ctx := api.WithNamespace(api.NewContext(), binding.Namespace)

	// default upstream scheduler passes pod.Name as binding.PodID
	podKey, err := makePodKey(ctx, binding.PodID)
	if err != nil {
		return err
	}

	b.api.Lock()
	defer b.api.Unlock()

	taskId, exists := b.api.taskForPod(podKey)
	if !exists {
		log.Infof("Could not resolve pod %s to task id", podKey)
		return noSuchPodErr
	}

	switch task, state := b.api.getTask(taskId); state {
	case statePending:
		return b.bind(ctx, binding, task)
	default:
		// in this case it's likely that the pod has been deleted between Schedule
		// and Bind calls
		log.Infof("No pending task for pod %s", podKey)
		return noSuchPodErr
	}
}

// assumes that: caller has acquired scheduler lock and that the PodTask is still pending
func (b *binder) bind(ctx api.Context, binding *api.Binding, task *PodTask) (err error) {
	// sanity check: ensure that the task hasAcceptedOffer(), it's possible that between
	// Schedule() and now that the offer for this task was rescinded or invalidated.
	// ((we should never see this here))
	if !task.hasAcceptedOffer() {
		return fmt.Errorf("task has not accepted a valid offer %v", task.ID)
	}

	// By this time, there is a chance that the slave is disconnected.
	offerId := task.GetOfferId()
	if offer, ok := b.api.offers().Get(offerId); !ok || offer.HasExpired() {
		// already rescinded or timed out or otherwise invalidated
		task.Offer.Release()
		task.ClearTaskInfo()
		return fmt.Errorf("failed prior to launchTask due to expired offer for task %v", task.ID)
	}

	if err = b.prepareTaskForLaunch(ctx, binding.Host, task); err == nil {
		log.V(2).Infof("Attempting to bind %v to %v", binding.PodID, binding.Host)
		if err = b.client.Post().Namespace(api.Namespace(ctx)).Resource("bindings").Body(binding).Do().Error(); err == nil {
			log.V(2).Infof("launching task : %v", task)
			if err = b.api.launchTask(task); err == nil {
				b.api.offers().Invalidate(offerId)
				task.Pod.Status.Host = binding.Host
				task.launched = true
				return
			}
		}
	}
	task.Offer.Release()
	task.ClearTaskInfo()
	return fmt.Errorf("Failed to launch task %v: %v", task.ID, err)
}

func (b *binder) prepareTaskForLaunch(ctx api.Context, machine string, task *PodTask) error {
	pod, err := b.client.Pods(api.Namespace(ctx)).Get(task.Pod.Name)
	if err != nil {
		return err
	}

	//HACK(jdef): adapted from https://github.com/GoogleCloudPlatform/kubernetes/blob/release-0.6/pkg/registry/pod/bound_pod_factory.go
	envVars, err := b.getServiceEnvironmentVariables(ctx)
	if err != nil {
		return err
	}

	boundPod := &api.BoundPod{}
	if err := api.Scheme.Convert(pod, boundPod); err != nil {
		return err
	}
	for ix, container := range boundPod.Spec.Containers {
		boundPod.Spec.Containers[ix].Env = append(container.Env, envVars...)
	}
	// Make a dummy self link so that references to this bound pod will work.
	boundPod.SelfLink = "/api/v1beta1/boundPods/" + boundPod.Name

	// update the boundPod here to pick up things like environment variables that
	// pod containers will use for service discovery. the kubelet-executor uses this
	// boundPod to instantiate the pods and this is the last update we make before
	// firing up the pod.
	task.TaskInfo.Data, err = yaml.Marshal(&boundPod)
	if err != nil {
		log.V(2).Infof("Failed to marshal the updated boundPod")
		return err
	}
	return nil
}

// getServiceEnvironmentVariables populates a list of environment variables that are use
// in the container environment to get access to services.
// HACK(jdef): adapted from https://github.com/GoogleCloudPlatform/kubernetes/blob/release-0.6/pkg/registry/pod/bound_pod_factory.go
func (b *binder) getServiceEnvironmentVariables(ctx api.Context) (result []api.EnvVar, err error) {
	var services *api.ServiceList
	if services, err = b.client.Services(api.Namespace(ctx)).List(labels.Everything()); err == nil {
		result = envvars.FromServices(services)
	}
	return
}

type kubeScheduler struct {
	api      SchedulerInterface
	podStore queue.FIFO
}

// Schedule implements the Scheduler interface of the Kubernetes.
// It returns the selectedMachine's name and error (if there's any).
func (k *kubeScheduler) Schedule(pod api.Pod, unused algorithm.MinionLister) (string, error) {
	log.Infof("Try to schedule pod %v\n", pod.Name)
	ctx := api.WithNamespace(api.NewDefaultContext(), pod.Namespace)

	// default upstream scheduler passes pod.Name as binding.PodID
	podKey, err := makePodKey(ctx, pod.Name)
	if err != nil {
		return "", err
	}

	k.api.Lock()
	defer k.api.Unlock()

	if taskID, ok := k.api.taskForPod(podKey); !ok {
		// There's a bit of a potential race here, a pod could have been yielded() but
		// and then before we get *here* it could be deleted. We use meta to index the pod
		// in the store since that's what k8s client/cache/reflector does.
		meta, err := meta.Accessor(&pod)
		if err != nil {
			log.Warningf("aborting Schedule, unable to understand pod object %+v", &pod)
			return "", noSuchPodErr
		}
		if deleted := k.podStore.Poll(meta.Name(), queue.DELETE_EVENT); deleted {
			// avoid scheduling a pod that's been deleted between yieldPod() and Schedule()
			log.Infof("aborting Schedule, pod has been deleted %+v", &pod)
			return "", noSuchPodErr
		}
		return k.doSchedule(k.api.registerPodTask(k.api.createPodTask(ctx, &pod)))
	} else {
		switch task, state := k.api.getTask(taskID); state {
		case statePending:
			if task.launched {
				return "", fmt.Errorf("task %s has already been launched, aborting schedule", taskID)
			} else {
				return k.doSchedule(task, nil)
			}
		default:
			return "", fmt.Errorf("task %s is not pending, nothing to schedule", taskID)
		}
	}
}

// Call ScheduleFunc and subtract some resources, returning the name of the machine the task is scheduled on
func (k *kubeScheduler) doSchedule(task *PodTask, err error) (string, error) {
	var offer PerishableOffer
	if err == nil {
		offer, err = k.api.algorithm()(k.api.offers(), k.api, task)
	}
	if err != nil {
		return "", err
	}
	slaveId := offer.Details().GetSlaveId().GetValue()
	if slave, ok := k.api.slaveFor(slaveId); !ok {
		// not much sense in Release()ing the offer here since its owner died
		offer.Release()
		k.api.offers().Invalidate(offer.Details().Id.GetValue())
		task.ClearTaskInfo()
		return "", fmt.Errorf("Slave disappeared (%v) while scheduling task %v", slaveId, task.ID)
	} else {
		task.FillTaskInfo(offer)
		return slave.HostName, nil
	}
}

type queuer struct {
	lock            sync.Mutex       // shared by condition variables of this struct
	podStore        queue.FIFO       // cache of pod updates to be processed
	podQueue        *queue.DelayFIFO // queue of pods to be scheduled
	deltaCond       sync.Cond        // pod changes are available for processing
	unscheduledCond sync.Cond        // there are unscheduled pods for processing
}

func newQueuer(store queue.FIFO) *queuer {
	q := &queuer{
		podQueue: queue.NewDelayFIFO(),
		podStore: store,
	}
	q.deltaCond.L = &q.lock
	q.unscheduledCond.L = &q.lock
	return q
}

// signal that there are probably pod updates waiting to be processed
func (q *queuer) updatesAvailable() {
	q.deltaCond.Broadcast()
}

// delete a pod from the to-be-scheduled queue
func (q *queuer) dequeue(id string) {
	q.podQueue.Delete(id)
}

// re-add a pod to the to-be-scheduled queue, will not overwrite existing pod data (that
// may have already changed).
func (q *queuer) requeue(pod *Pod) {
	// use KeepExisting in case the pod has already been updated (can happen if binding fails
	// due to constraint voilations); we don't want to overwrite a newer entry with stale data.
	q.podQueue.Add(pod, queue.KeepExisting)
	q.unscheduledCond.Broadcast()
}

// spawns a go-routine to watch for unscheduled pods and queue them up
// for scheduling. returns immediately.
func (q *queuer) Run() {
	go util.Forever(func() {
		log.Info("Watching for newly created pods")
		q.lock.Lock()
		defer q.lock.Unlock()

		for {
			// limit blocking here for short intervals so that scheduling
			// may proceed even if there have been no recent pod changes
			p := q.podStore.Await(enqueuePopTimeout)
			if p == nil {
				signalled := make(chan struct{})
				go func() {
					defer close(signalled)
					q.deltaCond.Wait()
				}()
				// we've yielded the lock
				select {
				case <-time.After(enqueueWaitTimeout):
					q.deltaCond.Broadcast() // abort Wait()
					<-signalled             // wait for lock re-acquisition
					log.V(4).Infoln("timed out waiting for a pod update")
				case <-signalled:
					// we've acquired the lock and there may be
					// changes for us to process now
				}
				continue
			}

			pod := p.(*Pod)
			if pod.Status.Host != "" {
				q.dequeue(pod.GetUID())
			} else {
				// use ReplaceExisting because we are always pushing the latest state
				now := time.Now()
				pod.deadline = &now
				q.podQueue.Offer(pod, queue.ReplaceExisting)
				q.unscheduledCond.Broadcast()
				log.V(3).Infof("queued pod for scheduling: %v", pod.Pod.Name)
			}
		}
	}, 1*time.Second)
}

// implementation of scheduling plugin's NextPod func; see k8s plugin/pkg/scheduler
func (q *queuer) yield() *api.Pod {
	log.V(2).Info("attempting to yield a pod")
	q.lock.Lock()
	defer q.lock.Unlock()

	for {
		// limit blocking here to short intervals so that we don't block the
		// enqueuer Run() routine for very long
		kpod := q.podQueue.Await(yieldPopTimeout)
		if kpod == nil {
			signalled := make(chan struct{})
			go func() {
				defer close(signalled)
				q.unscheduledCond.Wait()
			}()

			// lock is yielded at this point and we're going to wait for either
			// a timeout, or a signal that there's data
			select {
			case <-time.After(yieldWaitTimeout):
				q.unscheduledCond.Broadcast() // abort Wait()
				<-signalled                   // wait for the go-routine, and the lock
				log.V(4).Infoln("timed out waiting for a pod to yield")
			case <-signalled:
				// we have acquired the lock, and there
				// may be a pod for us to pop now
			}
			continue
		}

		pod := kpod.(*Pod).Pod
		if meta, err := meta.Accessor(pod); err != nil {
			log.Warningf("yield unable to understand pod object %+v, will skip", pod)
		} else if !q.podStore.Poll(meta.Name(), queue.POP_EVENT) {
			log.V(1).Infof("yield popped a transitioning pod, skipping: %+v", pod)
		} else if pod.Status.Host != "" {
			// should never happen if enqueuePods is filtering properly
			log.Warningf("yield popped an already-scheduled pod, skipping: %+v", pod)
		} else {
			return pod
		}
	}
}

type errorHandler struct {
	api     SchedulerInterface
	backoff *podBackoff
	qr      *queuer
}

// implementation of scheduling plugin's Error func; see plugin/pkg/scheduler
func (k *errorHandler) handleSchedulingError(pod *api.Pod, schedulingErr error) {

	if schedulingErr == noSuchPodErr {
		log.V(2).Infof("Not rescheduling non-existent pod %v", pod.Name)
		return
	}

	log.Infof("Error scheduling %v: %v; retrying", pod.Name, schedulingErr)
	defer util.HandleCrash()

	// default upstream scheduler passes pod.Name as binding.PodID
	ctx := api.WithNamespace(api.NewDefaultContext(), pod.Namespace)
	podKey, err := makePodKey(ctx, pod.Name)
	if err != nil {
		log.Errorf("Failed to construct pod key, aborting scheduling for pod %v: %v", pod.Name, err)
		return
	}

	k.backoff.gc()
	k.api.RLocker().Lock()
	defer k.api.RLocker().Unlock()

	taskId, exists := k.api.taskForPod(podKey)
	if !exists {
		// if we don't have a mapping here any more then someone deleted the pod
		log.V(2).Infof("Could not resolve pod to task, aborting pod reschdule: %s", podKey)
		return
	}

	switch task, state := k.api.getTask(taskId); state {
	case statePending:
		if task.launched {
			log.V(2).Infof("Skipping re-scheduling for already-launched pod %v", podKey)
			return
		}
		offersAvailable := queue.BreakChan(nil)
		if schedulingErr == noSuitableOffersErr {
			log.V(3).Infof("adding backoff breakout handler for pod %v", podKey)
			offersAvailable = queue.BreakChan(k.api.offers().Listen(podKey, func(offer *mesos.Offer) bool {
				k.api.RLocker().Lock()
				defer k.api.RLocker().Unlock()
				switch task, state := k.api.getTask(taskId); state {
				case statePending:
					return !task.launched && task.AcceptOffer(offer)
				}
				return false
			}))
		}
		delay := k.backoff.getBackoff(podKey)
		k.qr.requeue(&Pod{Pod: pod, delay: &delay, notify: offersAvailable})
	default:
		log.V(2).Infof("Task is no longer pending, aborting reschedule for pod %v", podKey)
	}
}

type deleter struct {
	api SchedulerInterface
	qr  *queuer
}

// currently monitors for "pod deleted" events, upon which handle()
// is invoked.
func (k *deleter) Run(updates <-chan queue.Entry) {
	go util.Forever(func() {
		for {
			entry := <-updates
			pod := entry.Value().(*Pod)
			if entry.Is(queue.DELETE_EVENT) {
				if err := k.deleteOne(pod); err != nil {
					log.Error(err)
				}
			} else if !entry.Is(queue.POP_EVENT) {
				k.qr.updatesAvailable()
			}
		}
	}, 1*time.Second)
}

func (k *deleter) deleteOne(pod *Pod) error {
	ctx := api.WithNamespace(api.NewDefaultContext(), pod.Namespace)
	podKey, err := makePodKey(ctx, pod.Name)
	if err != nil {
		return err
	}

	log.V(2).Infof("pod deleted: %v", podKey)

	// order is important here: we want to make sure we have the lock before
	// removing the pod from the scheduling queue. this makes the concurrent
	// execution of scheduler-error-handling and delete-handling easier to
	// reason about.
	k.api.Lock()
	defer k.api.Unlock()

	// prevent the scheduler from attempting to pop this; it's also possible that
	// it's concurrently being scheduled (somewhere between pod scheduling and
	// binding) - if so, then we'll end up removing it from pendingTasks which
	// will abort Bind()ing
	k.qr.dequeue(pod.GetUID())

	taskId, exists := k.api.taskForPod(podKey)
	if !exists {
		log.V(2).Infof("Could not resolve pod '%s' to task id", podKey)
		return noSuchPodErr
	}

	// determine if the task has already been launched to mesos, if not then
	// cleanup is easier (unregister) since there's no state to sync
	switch task, state := k.api.getTask(taskId); state {
	case statePending:
		if !task.launched {
			// we've been invoked in between Schedule() and Bind()
			if task.hasAcceptedOffer() {
				task.Offer.Release()
				task.ClearTaskInfo()
			}
			k.api.unregisterPodTask(task)
			return nil
		}
		fallthrough
	case stateRunning:
		// signal to watchers that the related pod is going down
		task.deleted = true
		task.Pod.Status.Host = ""
		return k.api.killTask(taskId)
	default:
		log.Warningf("cannot kill pod '%s': task not found %v", podKey, taskId)
		return noSuchTaskErr
	}
}

// Create creates a scheduler plugin and all supporting background functions.
func (k *KubernetesScheduler) NewPluginConfig() *plugin.Config {

	// Watch and queue pods that need scheduling.
	updates := make(chan queue.Entry, defaultUpdatesBacklog)
	podStore := &podStoreAdapter{queue.NewHistorical(updates)}
	cache.NewReflector(createAllPodsLW(k.client), &api.Pod{}, podStore).Run()

	// lock that guards critial sections that involve transferring pods from
	// the store (cache) to the scheduling queue; its purpose is to maintain
	// an ordering (vs interleaving) of operations that's easier to reason about.
	kapi := &k8smScheduler{k}
	q := newQueuer(podStore)
	podDeleter := &deleter{
		api: kapi,
		qr:  q,
	}
	podDeleter.Run(updates)
	q.Run()

	eh := &errorHandler{
		api: kapi,
		backoff: &podBackoff{
			perPodBackoff: map[string]*backoffEntry{},
			clock:         realClock{},
		},
		qr: q,
	}
	return &plugin.Config{
		MinionLister: nil,
		Algorithm: &kubeScheduler{
			api:      kapi,
			podStore: podStore,
		},
		Binder: &binder{
			api:    kapi,
			client: k.client,
		},
		NextPod: q.yield,
		Error:   eh.handleSchedulingError,
	}
}

type listWatch struct {
	client        *client.Client
	fieldSelector labels.Selector
	resource      string
}

func (lw *listWatch) List() (runtime.Object, error) {
	return lw.client.
		Get().
		Resource(lw.resource).
		SelectorParam("fields", lw.fieldSelector).
		Do().
		Get()
}

func (lw *listWatch) Watch(resourceVersion string) (watch.Interface, error) {
	return lw.client.
		Get().
		Prefix("watch").
		Resource(lw.resource).
		SelectorParam("fields", lw.fieldSelector).
		Param("resourceVersion", resourceVersion).
		Watch()
}

// createAllPodsLW returns a listWatch that finds all pods
func createAllPodsLW(cl *client.Client) *listWatch {
	return &listWatch{
		client:        cl,
		fieldSelector: labels.Everything(),
		resource:      "pods",
	}
}

// Consumes *api.Pod, produces *Pod; the k8s reflector wants to push *api.Pod
// objects at us, but we want to store more flexible (Pod) type defined in
// this package. The adapter implementation facilitates this. It's a little
// hackish since the object type going in is different than the object type
// coming out -- you've been warned.
type podStoreAdapter struct {
	queue.FIFO
}

func (psa *podStoreAdapter) Add(id string, obj interface{}) {
	pod := obj.(*api.Pod)
	psa.FIFO.Add(id, &Pod{Pod: pod})
}

func (psa *podStoreAdapter) Update(id string, obj interface{}) {
	pod := obj.(*api.Pod)
	psa.FIFO.Update(id, &Pod{Pod: pod})
}

// Replace will delete the contents of the store, using instead the
// given map. This store implementation does NOT take ownership of the map.
func (psa *podStoreAdapter) Replace(idToObj map[string]interface{}) {
	newmap := map[string]interface{}{}
	for k, v := range idToObj {
		pod := v.(*api.Pod)
		newmap[k] = &Pod{Pod: pod}
	}
	psa.FIFO.Replace(newmap)
}
