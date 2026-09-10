package route

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func TestK8sEventRecorder_WarnCreatesEvent(t *testing.T) {
	client := k8sfake.NewSimpleClientset()
	rec := NewK8sEventRecorder(client)

	rec.Warn(testNamespace, "tenant1-pooler-b", reasonDuplicateHostname, "hostname already claimed")

	events, err := client.CoreV1().Events(testNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events.Items) != 1 {
		t.Fatalf("got %d events, want 1", len(events.Items))
	}
	got := events.Items[0]
	if got.Reason != reasonDuplicateHostname {
		t.Errorf("Reason = %q, want %q", got.Reason, reasonDuplicateHostname)
	}
	if got.Type != corev1.EventTypeWarning {
		t.Errorf("Type = %q, want Warning", got.Type)
	}
	if got.InvolvedObject.Name != "tenant1-pooler-b" || got.InvolvedObject.Namespace != testNamespace {
		t.Errorf("InvolvedObject = %+v, want name=tenant1-pooler-b namespace=%s", got.InvolvedObject, testNamespace)
	}
	if got.InvolvedObject.Kind != "Pooler" {
		t.Errorf("InvolvedObject.Kind = %q, want Pooler", got.InvolvedObject.Kind)
	}
}
