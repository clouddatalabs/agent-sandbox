// Copyright 2026 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controllers

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
	asmetrics "sigs.k8s.io/agent-sandbox/internal/metrics"
)

func TestSandboxClaimSharedSandboxMountsHappyPath(t *testing.T) {
	template := newSharedSandboxMountsTemplate()
	claim := &extensionsv1alpha1.SandboxClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-claim",
			Namespace: "default",
			UID:       "claim-uid",
			Annotations: map[string]string{
				sharedSandboxMountsAnnotation: `[{"sourceSandboxId":"source-a","sourcePath":"/workspace/reports/out.txt","ignored":"field"}]`,
			},
		},
		Spec: extensionsv1alpha1.SandboxClaimSpec{
			TemplateRef: extensionsv1alpha1.SandboxTemplateRef{Name: template.Name},
		},
	}
	scheme := newScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(template, claim).
		WithStatusSubresource(claim).
		Build()
	reconciler := &SandboxClaimReconciler{
		Client:   fakeClient,
		Scheme:   scheme,
		Recorder: events.NewFakeRecorder(10),
		Tracer:   asmetrics.NewNoOp(),
	}

	ctx := context.Background()
	_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace}})
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	var sandbox sandboxv1alpha1.Sandbox
	if err := fakeClient.Get(ctx, types.NamespacedName{Name: claim.Name, Namespace: claim.Namespace}, &sandbox); err != nil {
		t.Fatalf("get sandbox: %v", err)
	}

	expectedMounts := []corev1.VolumeMount{
		{
			Name:        "persist",
			MountPath:   "/workspace",
			SubPathExpr: "$(RESOLVE_ORG_ID)/sandboxes/$(RESOLVE_AGENT_ID)",
		},
		{
			Name:        "persist",
			ReadOnly:    true,
			MountPath:   "/workspace/shared_sandboxes/source-a/reports/out.txt",
			SubPathExpr: "$(RESOLVE_ORG_ID)/sandboxes/source-a/reports/out.txt",
		},
	}
	if diff := cmp.Diff(expectedMounts, sandbox.Spec.PodTemplate.Spec.Containers[0].VolumeMounts); diff != "" {
		t.Fatalf("volume mounts mismatch (-want +got):\n%s", diff)
	}
}

func TestValidateSharedSandboxMounts(t *testing.T) {
	validMounts, err := validateSharedSandboxMounts([]sharedSandboxMountAnnotation{
		{SourceSandboxID: "source-a", SourcePath: "/workspace/reports/out.txt"},
	})
	if err != nil {
		t.Fatalf("validate valid mount: %v", err)
	}
	if len(validMounts) != 1 {
		t.Fatalf("expected 1 mount, got %d", len(validMounts))
	}
	if validMounts[0].mountPath != "/workspace/shared_sandboxes/source-a/reports/out.txt" {
		t.Fatalf("unexpected mountPath: %q", validMounts[0].mountPath)
	}
	if validMounts[0].subPathExpr != "$(RESOLVE_ORG_ID)/sandboxes/source-a/reports/out.txt" {
		t.Fatalf("unexpected subPathExpr: %q", validMounts[0].subPathExpr)
	}

	_, err = validateSharedSandboxMounts([]sharedSandboxMountAnnotation{
		{SourceSandboxID: "source-a", SourcePath: "/workspace/shared_sandboxes/source-b"},
	})
	if err == nil {
		t.Fatal("expected reshare path to be rejected")
	}
}

func newSharedSandboxMountsTemplate() *extensionsv1alpha1.SandboxTemplate {
	return &extensionsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "test-template", Namespace: "default"},
		Spec: extensionsv1alpha1.SandboxTemplateSpec{
			PodTemplate: sandboxv1alpha1.PodTemplate{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "sandbox",
							Image: "test-image",
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:        "persist",
									MountPath:   "/workspace",
									SubPathExpr: "$(RESOLVE_ORG_ID)/sandboxes/$(RESOLVE_AGENT_ID)",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "persist",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "shared-pvc"},
							},
						},
					},
				},
			},
		},
	}
}
