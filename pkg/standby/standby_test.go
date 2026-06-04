/*
Copyright 2026 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package standby

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type stubDetector struct {
	isStandby bool
	err       error
}

type stubReconciler struct {
	called *bool
	result reconcile.Result
	err    error
}

type errorReader struct {
	err error
}

func (d stubDetector) IsStandby(context.Context) (bool, error) {
	return d.isStandby, d.err
}

func (r stubReconciler) Reconcile(context.Context, reconcile.Request) (reconcile.Result, error) {
	*r.called = true
	return r.result, r.err
}

func (r errorReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return r.err
}

func (r errorReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return r.err
}

func TestConfigMapDetectorIsStandby(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		objects   []client.Object
		want      bool
		wantError bool
	}{
		{
			name: "ConfigMap exists",
			objects: []client.Object{
				newStandbyConfigMap(),
			},
			want: true,
		},
		{
			name: "ConfigMap does not exist",
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tt.objects...).Build()
			got, err := NewConfigMapDetector(reader).IsStandby(context.Background())
			if (err != nil) != tt.wantError {
				t.Fatalf("IsStandby() error = %v, wantError %v", err, tt.wantError)
			}
			if got != tt.want {
				t.Fatalf("IsStandby() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestConfigMapDetectorReturnsGetError(t *testing.T) {
	getErr := errors.New("get failed")
	got, err := NewConfigMapDetector(errorReader{err: getErr}).IsStandby(context.Background())
	if !errors.Is(err, getErr) {
		t.Fatalf("IsStandby() error = %v, want %v", err, getErr)
	}
	if got {
		t.Fatal("IsStandby() = true, want false")
	}
}

func TestWrapReconciler(t *testing.T) {
	detectorErr := errors.New("detector failed")
	innerErr := errors.New("inner failed")

	tests := []struct {
		name        string
		detector    Detector
		innerResult reconcile.Result
		innerErr    error
		wantCalled  bool
		wantResult  reconcile.Result
		wantErr     error
	}{
		{
			name:       "standby skips inner reconciler",
			detector:   stubDetector{isStandby: true},
			wantCalled: false,
			wantResult: reconcile.Result{RequeueAfter: RequeueAfter},
		},
		{
			name:        "active calls inner reconciler",
			detector:    stubDetector{},
			innerResult: reconcile.Result{Requeue: true},
			wantCalled:  true,
			wantResult:  reconcile.Result{Requeue: true},
		},
		{
			name:       "detector error skips inner reconciler",
			detector:   stubDetector{err: detectorErr},
			wantCalled: false,
			wantErr:    detectorErr,
		},
		{
			name:       "inner error is returned when active",
			detector:   stubDetector{},
			innerErr:   innerErr,
			wantCalled: true,
			wantErr:    innerErr,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			wrapped := WrapReconciler(tt.detector, "test", stubReconciler{
				called: &called,
				result: tt.innerResult,
				err:    tt.innerErr,
			})

			got, err := wrapped.Reconcile(context.Background(), reconcile.Request{})
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Reconcile() error = %v, want %v", err, tt.wantErr)
			}
			if called != tt.wantCalled {
				t.Fatalf("inner called = %v, want %v", called, tt.wantCalled)
			}
			if got != tt.wantResult {
				t.Fatalf("Reconcile() result = %v, want %v", got, tt.wantResult)
			}
		})
	}
}

func TestWrapClusterNamedReconciler(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	globalObject := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: metav1.NamespaceDefault,
			Name:      "global",
			Labels:    map[string]string{clusterv1.ClusterNameLabel: GlobalClusterName},
		},
	}
	businessObject := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: metav1.NamespaceDefault,
			Name:      "business",
			Labels:    map[string]string{clusterv1.ClusterNameLabel: "business"},
		},
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(globalObject, businessObject).Build()

	tests := []struct {
		name       string
		detector   Detector
		request    reconcile.Request
		wantCalled bool
		wantResult reconcile.Result
		wantErr    error
	}{
		{
			name:       "standby allows global cluster object",
			detector:   stubDetector{isStandby: true},
			request:    reconcile.Request{NamespacedName: types.NamespacedName{Namespace: metav1.NamespaceDefault, Name: "global"}},
			wantCalled: true,
		},
		{
			name:       "standby skips business cluster object",
			detector:   stubDetector{isStandby: true},
			request:    reconcile.Request{NamespacedName: types.NamespacedName{Namespace: metav1.NamespaceDefault, Name: "business"}},
			wantCalled: false,
			wantResult: reconcile.Result{RequeueAfter: RequeueAfter},
		},
		{
			name:       "active calls inner reconciler",
			detector:   stubDetector{},
			request:    reconcile.Request{NamespacedName: types.NamespacedName{Namespace: metav1.NamespaceDefault, Name: "business"}},
			wantCalled: true,
		},
		{
			name:       "standby lets not found object reach inner reconciler",
			detector:   stubDetector{isStandby: true},
			request:    reconcile.Request{NamespacedName: types.NamespacedName{Namespace: metav1.NamespaceDefault, Name: "missing"}},
			wantCalled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			wrapped := WrapClusterNamedReconciler(tt.detector, reader, "test", func() client.Object { return &corev1.ConfigMap{} }, ClusterNameFromLabel, stubReconciler{called: &called})

			got, err := wrapped.Reconcile(context.Background(), tt.request)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Reconcile() error = %v, want %v", err, tt.wantErr)
			}
			if called != tt.wantCalled {
				t.Fatalf("inner called = %v, want %v", called, tt.wantCalled)
			}
			if got != tt.wantResult {
				t.Fatalf("Reconcile() result = %v, want %v", got, tt.wantResult)
			}
		})
	}
}

func newStandbyConfigMap() client.Object {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ConfigMapName,
			Namespace: ConfigMapNamespace,
		},
	}
}
