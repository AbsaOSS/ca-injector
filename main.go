package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
	admv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func secsSince(t time.Time) float64 {
	return float64(time.Since(t)) / float64(time.Second)
}

func first(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

const (
	label      = "microcumul.us/injectssl"
	volumeName = "microcumulus-injected-ssl"
)

type p struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}
type m map[string]interface{}

var (
	ctrDeletes = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "certinjector_pods_deleted",
		Help: "The number of pods deleted by the certinjector pod",
	}, []string{"namespace", "name"})

	ctrPatches = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "certinjector_pods_mutated",
		Help: "The number of pods mutated by the certinjector webhook",
	}, []string{"namespace", "name"})
)

func main() {
	setupConfig()

	http.Handle("/metrics", promhttp.Handler())
	http.Handle("/pods", admitFunc(func(ar admv1.AdmissionReview) (res *admv1.AdmissionResponse, err error) {
		var pod corev1.Pod
		obj, _, err := codecs.UniversalDeserializer().Decode(ar.Request.Object.Raw, nil, &pod)
		if err != nil {
			if err != nil {
				lg.WithError(err).Error("could not deserialize pod spec")
				return nil, err
			}
		}

		lg := lg.WithFields(logrus.Fields{
			"ar.Request.Name":                        ar.Request.Name,
			"ar.Request.Namespace":                   ar.Request.Namespace,
			"pod.Name":                               pod.Name,
			"pod.Namespace":                          pod.Namespace,
			"pod.CreationTimestamp":                  pod.CreationTimestamp.Time,
			"obj.GetObjectKind().GroupVersionKind()": obj.GetObjectKind().GroupVersionKind(),
		})

		if pod.Annotations[label] == "" {
			lg.Info("allowing")
			return &admv1.AdmissionResponse{
				Allowed: true,
			}, nil
		}
		lg.Info("will patch")

		var patch []p
		if pod.Spec.Volumes == nil {
			patch = append(patch, p{
				Op:    "add",
				Path:  "/spec/volumes",
				Value: []interface{}{}, // add array if none
			})
		}

		// TODO add documentation that the secret needs to have `ca.crt` key/value
		patch = append(patch, p{
			Op:   "add",
			Path: "/spec/volumes/-",
			Value: m{
				"name": volumeName,
				"secret": m{
					"secretName": pod.Annotations[label],
				},
			},
		})

		for i, ctr := range pod.Spec.Containers {
			ps := []p{{
				Op:   "add",
				Path: fmt.Sprintf("/spec/containers/%d/env/-", i),
				Value: m{
					"name":  "SSL_CERT_FILE",
					"value": "/ssl/ca.crt",
				},
			}, {
				Op:   "add",
				Path: fmt.Sprintf("/spec/containers/%d/env/-", i),
				Value: m{
					"name":  "NODE_EXTRA_CA_CERTS",
					"value": "/ssl/ca.crt",
				},
			}, {
				Op:   "add",
				Path: fmt.Sprintf("/spec/containers/%d/volumeMounts/-", i),
				Value: m{
					"name":      volumeName,
					"mountPath": "/ssl",
					"readOnly":  true,
				},
			}}

			if ctr.Env == nil {
				ps = append([]p{{
					Op:    "add",
					Path:  fmt.Sprintf("/spec/containers/%d/env", i),
					Value: []interface{}{}, //add the array if none
				}}, ps...)
			}
			if len(ctr.VolumeMounts) == 0 {
				ps = append([]p{{
					Op:    "add",
					Path:  fmt.Sprintf("/spec/containers/%d/volumeMounts", i),
					Value: []interface{}{}, //add the array if none
				}}, ps...)
			}

			patch = append(patch, ps...)
		}

		ctrPatches.WithLabelValues(pod.Namespace, pod.Name).Inc()
		lg.WithField("patch", patch).Info("patching")

		bs, _ := json.Marshal(patch)

		pt := admv1.PatchTypeJSONPatch
		return &admv1.AdmissionResponse{
			Allowed:   true,
			Patch:     bs,
			PatchType: &pt,
			Result: &metav1.Status{
				Message: "modified",
			},
		}, nil
	}))

	conf, err := rest.InClusterConfig()
	if err != nil {
		log.Fatal(err)
	}

	go func() {
		time.Sleep(5 * time.Second)

		f := false
		for {
			if f {
				time.Sleep(60 * time.Second)
			}
			f = true

			ctx := context.TODO()
			cs := kubernetes.NewForConfigOrDie(conf)
			pods, err := cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
			if err != nil {
				logrus.WithError(err).Fatal("error listing pods")
			}

		items:
			for _, pod := range pods.Items {
				lg := lg.WithFields(logrus.Fields{
					"pod.Name":      pod.Name,
					"pod.Namespace": pod.Namespace,
				})

				or := corev1.ObjectReference{
					Kind:            pod.Kind,
					Namespace:       pod.Namespace,
					Name:            pod.Name,
					UID:             pod.UID,
					APIVersion:      pod.APIVersion,
					ResourceVersion: pod.ResourceVersion,
				}

				if len(pod.OwnerReferences) > 0 {
					or = corev1.ObjectReference{
						Kind:       pod.OwnerReferences[0].Kind,
						Namespace:  pod.Namespace,
						Name:       pod.OwnerReferences[0].Name,
						UID:        pod.OwnerReferences[0].UID,
						APIVersion: pod.OwnerReferences[0].APIVersion,
					}
				}

				cs.CoreV1().Events(pod.Namespace).Create(ctx, &corev1.Event{
					InvolvedObject: or,
					Reason:         "Deleting pod",
					Message:        fmt.Sprintf("pod annotation on %q has not been applied by ca-injector mutatingadmissionwebhook", pod.Name),
				}, metav1.CreateOptions{})
				secret := pod.Annotations[label]
				if secret == "" {
					continue
				}

				// Look for well-known volume in list of mounts
				for _, vol := range pod.Spec.Volumes {
					if vol.Secret != nil && vol.Secret.SecretName == secret {
						continue items
					}
				}

				lg.Info("deleting pod; CA env and mount not found")
				ctrDeletes.WithLabelValues(pod.Namespace, pod.Name).Inc()
				err := cs.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})
				if err != nil {
					logrus.WithError(err).WithField("pod", pod.Name).Error("error deleting pod")
				}
			}
		}
	}()

	s := http.Server{
		Addr:    ":8443",
		Handler: http.DefaultServeMux,
	}

	ch := make(chan os.Signal, 2)
	signal.Notify(ch, syscall.SIGTERM, os.Interrupt)
	go func() {
		i := 0
		for range ch {
			i++
			if i > 1 {
				os.Exit(1)
			}
			s.Shutdown(context.Background())
		}
	}()

	lg.Info("listening")

	lg.Fatal(s.ListenAndServeTLS("/cert/tls.crt", "/cert/tls.key"))
}
