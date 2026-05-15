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
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"unicode"

	corev1 "k8s.io/api/core/v1"

	sandboxv1alpha1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	extensionsv1alpha1 "sigs.k8s.io/agent-sandbox/extensions/api/v1alpha1"
)

const (
	sharedSandboxMountsAnnotation = "resolve.ai/shared-sandbox-mounts.v1"

	workspacePath       = "/workspace"
	sharedSandboxesPath = "/workspace/shared_sandboxes"
	resolveOrgIDExpr    = "$(RESOLVE_ORG_ID)"
)

type sharedSandboxMountAnnotation struct {
	SourceSandboxID string `json:"sourceSandboxId"`
	SourcePath      string `json:"sourcePath"`
}

type sharedSandboxMount struct {
	mountPath   string
	subPathExpr string
}

func parseSharedSandboxMountsAnnotation(claim *extensionsv1alpha1.SandboxClaim) ([]sharedSandboxMount, error) {
	if claim.Annotations == nil {
		return nil, nil
	}
	raw, ok := claim.Annotations[sharedSandboxMountsAnnotation]
	if !ok {
		return nil, nil
	}

	var annotatedMounts []sharedSandboxMountAnnotation
	if err := json.Unmarshal([]byte(raw), &annotatedMounts); err != nil {
		return nil, fmt.Errorf("decode %s: %w", sharedSandboxMountsAnnotation, err)
	}

	return validateSharedSandboxMounts(annotatedMounts)
}

func validateSharedSandboxMounts(annotatedMounts []sharedSandboxMountAnnotation) ([]sharedSandboxMount, error) {
	if len(annotatedMounts) == 0 {
		return nil, nil
	}

	mounts := make([]sharedSandboxMount, 0, len(annotatedMounts))
	seenMountPaths := make(map[string]struct{}, len(annotatedMounts))
	for i, mount := range annotatedMounts {
		validatedMount, err := validateSharedSandboxMount(mount)
		if err != nil {
			return nil, fmt.Errorf("mount %d: %w", i, err)
		}
		if _, ok := seenMountPaths[validatedMount.mountPath]; ok {
			return nil, fmt.Errorf("mount %d: duplicate mountPath %q", i, validatedMount.mountPath)
		}
		for existingMountPath := range seenMountPaths {
			if isNestedPath(existingMountPath, validatedMount.mountPath) || isNestedPath(validatedMount.mountPath, existingMountPath) {
				return nil, fmt.Errorf("mount %d: mountPath %q overlaps %q", i, validatedMount.mountPath, existingMountPath)
			}
		}
		seenMountPaths[validatedMount.mountPath] = struct{}{}
		mounts = append(mounts, validatedMount)
	}

	return mounts, nil
}

func validateSharedSandboxMount(mount sharedSandboxMountAnnotation) (sharedSandboxMount, error) {
	if !isSafeSandboxIDPathSegment(mount.SourceSandboxID) {
		return sharedSandboxMount{}, fmt.Errorf("sourceSandboxId %q must be a safe path segment", mount.SourceSandboxID)
	}
	if containsUnsafePathCharacter(mount.SourcePath) {
		return sharedSandboxMount{}, fmt.Errorf("sourcePath %q contains unsupported characters", mount.SourcePath)
	}

	cleanSourcePath := path.Clean(mount.SourcePath)
	if cleanSourcePath != workspacePath && !strings.HasPrefix(cleanSourcePath, workspacePath+"/") {
		return sharedSandboxMount{}, fmt.Errorf("sourcePath %q must be under %s", mount.SourcePath, workspacePath)
	}
	if cleanSourcePath == sharedSandboxesPath || strings.HasPrefix(cleanSourcePath, sharedSandboxesPath+"/") {
		return sharedSandboxMount{}, fmt.Errorf("sourcePath %q cannot be under %s", mount.SourcePath, sharedSandboxesPath)
	}

	relativeSourcePath := strings.TrimPrefix(cleanSourcePath, workspacePath)
	relativeSourcePath = strings.TrimPrefix(relativeSourcePath, "/")

	return sharedSandboxMount{
		mountPath:   path.Join(sharedSandboxesPath, mount.SourceSandboxID, relativeSourcePath),
		subPathExpr: path.Join(resolveOrgIDExpr, "sandboxes", mount.SourceSandboxID, relativeSourcePath),
	}, nil
}

func isSafeSandboxIDPathSegment(value string) bool {
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, "/$") {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func containsUnsafePathCharacter(value string) bool {
	if value == "" || strings.Contains(value, "$") {
		return true
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func isNestedPath(parentPath, childPath string) bool {
	return strings.HasPrefix(childPath, parentPath+"/")
}

func applySharedSandboxMountsToSandboxSpec(spec *sandboxv1alpha1.SandboxSpec, mounts []sharedSandboxMount) error {
	if len(mounts) == 0 {
		return nil
	}

	mountedContainerCount := 0
	for i := range spec.PodTemplate.Spec.Containers {
		workspaceVolumeName, ok := findWorkspaceVolumeName(spec.PodTemplate.Spec.Containers[i].VolumeMounts)
		if !ok {
			continue
		}
		if err := validateSharedSandboxVolume(spec, workspaceVolumeName); err != nil {
			return err
		}
		if err := appendSharedSandboxMounts(&spec.PodTemplate.Spec.Containers[i], workspaceVolumeName, mounts); err != nil {
			return err
		}
		mountedContainerCount++
	}
	if mountedContainerCount == 0 {
		return fmt.Errorf("no container mounts %s", workspacePath)
	}

	return nil
}

func findWorkspaceVolumeName(volumeMounts []corev1.VolumeMount) (string, bool) {
	for _, volumeMount := range volumeMounts {
		if volumeMount.MountPath == workspacePath {
			return volumeMount.Name, true
		}
	}
	return "", false
}

func validateSharedSandboxVolume(spec *sandboxv1alpha1.SandboxSpec, volumeName string) error {
	for _, volume := range spec.PodTemplate.Spec.Volumes {
		if volume.Name != volumeName {
			continue
		}
		if volume.PersistentVolumeClaim == nil {
			return fmt.Errorf("workspace volume %q is not a persistentVolumeClaim", volumeName)
		}
		return nil
	}
	return fmt.Errorf("workspace volume %q not found", volumeName)
}

func appendSharedSandboxMounts(container *corev1.Container, volumeName string, mounts []sharedSandboxMount) error {
	existingMountPaths := make(map[string]struct{}, len(container.VolumeMounts))
	for _, volumeMount := range container.VolumeMounts {
		existingMountPaths[volumeMount.MountPath] = struct{}{}
	}

	for _, mount := range mounts {
		if _, ok := existingMountPaths[mount.mountPath]; ok {
			return fmt.Errorf("container %q already has mountPath %q", container.Name, mount.mountPath)
		}
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
			Name:        volumeName,
			ReadOnly:    true,
			MountPath:   mount.mountPath,
			SubPathExpr: mount.subPathExpr,
		})
		existingMountPaths[mount.mountPath] = struct{}{}
	}

	return nil
}
