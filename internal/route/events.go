package route

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// eventComponent identifies pg-proxy as the source of the events it emits.
const eventComponent = "pg-proxy"

// K8sEventRecorder implements EventRecorder by creating core/v1 Events
// against the involved Pooler. A typed clientset is used here (unlike the
// dynamic client used for Poolers themselves) because Event is a stable
// core resource — this doesn't pull in the CNPG module (design §7.5).
type K8sEventRecorder struct {
	client kubernetes.Interface
}

// NewK8sEventRecorder returns an EventRecorder backed by the given clientset.
func NewK8sEventRecorder(client kubernetes.Interface) *K8sEventRecorder {
	return &K8sEventRecorder{client: client}
}

// Warn creates a Warning event on the named Pooler. A failure to create the
// event is logged, not returned — a missing k8s Event must never block
// route table rebuilds.
func (r *K8sEventRecorder) Warn(namespace, name, reason, message string) {
	now := metav1.NewTime(time.Now())
	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("%s-%s-", name, strings.ToLower(reason)),
			Namespace:    namespace,
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:       "Pooler",
			APIVersion: "postgresql.cnpg.io/v1",
			Namespace:  namespace,
			Name:       name,
		},
		Reason:         reason,
		Message:        message,
		Type:           corev1.EventTypeWarning,
		FirstTimestamp: now,
		LastTimestamp:  now,
		Count:          1,
		Source:         corev1.EventSource{Component: eventComponent},
	}

	if _, err := r.client.CoreV1().Events(namespace).Create(context.Background(), event, metav1.CreateOptions{}); err != nil {
		log.Printf("route: failed to emit %s event for pooler %s/%s: %v", reason, namespace, name, err)
	}
}
