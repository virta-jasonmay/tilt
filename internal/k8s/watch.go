package k8s

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/pkg/errors"
	v1 "k8s.io/api/core/v1"
	apiErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"

	"github.com/windmilleng/tilt/pkg/logger"
)

var PodGVR = v1.SchemeGroupVersion.WithResource("pods")
var ServiceGVR = v1.SchemeGroupVersion.WithResource("services")
var EventGVR = v1.SchemeGroupVersion.WithResource("events")

type watcherFactory func(namespace string) watcher
type watcher interface {
	Watch(options metav1.ListOptions) (watch.Interface, error)
}

func (kCli K8sClient) makeWatcher(f watcherFactory, ls labels.Selector) (watch.Interface, Namespace, error) {
	// passing "" gets us all namespaces
	w := f("")
	if w == nil {
		return nil, "", nil
	}

	watcher, err := w.Watch(metav1.ListOptions{LabelSelector: ls.String()})
	if err == nil {
		return watcher, "", nil
	}

	// If the request failed, we might be able to recover.
	statusErr, isStatusErr := err.(*apiErrors.StatusError)
	if !isStatusErr {
		return nil, "", err
	}

	status := statusErr.ErrStatus
	if status.Code == http.StatusForbidden {
		// If this is a forbidden error, maybe the user just isn't allowed to watch this namespace.
		// Let's narrow our request to just the config namespace, and see if that helps.
		w := f(kCli.configNamespace.String())
		if w == nil {
			return nil, "", nil
		}

		watcher, err := w.Watch(metav1.ListOptions{LabelSelector: ls.String()})
		if err == nil {
			return watcher, kCli.configNamespace, nil
		}

		// ugh, it still failed. return the original error.
	}
	return nil, "", fmt.Errorf("%s, Reason: %s, Code: %d", status.Message, status.Reason, status.Code)
}

func (kCli K8sClient) makeInformer(
	ctx context.Context,
	gvr schema.GroupVersionResource,
	ls labels.Selector) (cache.SharedInformer, error) {
	// HACK(dmiller): There's no way to get errors out of an informer. See https://github.com/kubernetes/client-go/issues/155
	// In the meantime, at least to get authorization and some other errors let's try to set up a watcher and then just
	// throw it away.
	watcher, ns, err := kCli.makeWatcher(func(ns string) watcher {
		return kCli.dynamic.Resource(gvr).Namespace(ns)
	}, ls)
	if err != nil {
		return nil, errors.Wrap(err, "makeInformer")
	}
	watcher.Stop()

	options := []informers.SharedInformerOption{}
	if !ls.Empty() {
		options = append(options, informers.WithTweakListOptions(func(o *metav1.ListOptions) {
			o.LabelSelector = ls.String()
		}))
	}
	if ns != "" {
		options = append(options, informers.WithNamespace(ns.String()))
	}

	factory := informers.NewSharedInformerFactoryWithOptions(kCli.clientset, 5*time.Second, options...)
	resFactory, err := factory.ForResource(gvr)
	if err != nil {
		return nil, errors.Wrap(err, "makeInformer")
	}

	return resFactory.Informer(), nil
}

func (kCli K8sClient) WatchResource(ctx context.Context, gvr schema.GroupVersionResource, ls labels.Selector) (<-chan interface{}, error) {
	informer, err := kCli.makeInformer(ctx, gvr, ls)
	if err != nil {
		return nil, errors.Wrap(err, "WatchResource")
	}

	ch := make(chan interface{})
	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pod, ok := obj.(*v1.Pod)
			if ok {
				FixContainerStatusImages(pod)
			}
			ch <- obj
		},
		DeleteFunc: func(obj interface{}) {
			pod, ok := obj.(*v1.Pod)
			if ok {
				FixContainerStatusImages(pod)
			}
			ch <- obj
		},
		UpdateFunc: func(oldObj interface{}, newObj interface{}) {
			newPod, ok := newObj.(*v1.Pod)
			if ok {
				FixContainerStatusImages(newPod)
			}

			newEvent, ok := newObj.(*v1.Event)
			if ok {
				oldEvent, ok := oldObj.(*v1.Event)
				// OnUpdate is regularly called when a re-list happens.
				//
				// For events, this ends up spamming us with tons of events even if nothing has changed.
				// To improve throughput a bit, we filter out events where count hasn't changed.
				//
				// For more info on this behavior, see:
				// https://godoc.org/k8s.io/client-go/tools/cache#ResourceEventHandler
				if ok && oldEvent.Count >= newEvent.Count {
					return
				}
			}

			ch <- newObj
		},
	})

	go runInformer(ctx, gvr.Resource, informer)

	return ch, nil
}

func runInformer(ctx context.Context, name string, informer cache.SharedInformer) {
	originalDuration := 3 * time.Second
	originalBackoff := wait.Backoff{
		Steps:    1000,
		Duration: originalDuration,
		Factor:   3.0,
		Jitter:   0.5,
		Cap:      time.Hour,
	}
	backoff := originalBackoff
	_ = informer.SetDropWatchHandler(func(err error, doneCh <-chan struct{}) {
		sleepTime := originalDuration
		if err != nil {
			sleepTime = backoff.Step()
			logger.Get(ctx).Warnf("Pausing k8s %s watcher for %s: %v",
				name,
				sleepTime.Truncate(time.Second),
				err)
		} else {
			backoff = originalBackoff
		}

		select {
		case <-doneCh:
		case <-time.After(sleepTime):
		}
	})
	informer.Run(ctx.Done())
}
