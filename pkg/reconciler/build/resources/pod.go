/*
Copyright 2018 The Knative Authors

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

// Package resources provides methods to convert a Build CRD to a k8s Pod
// resource.
package resources

import (
	"flag"
	"fmt"
	"path/filepath"
	"strconv"

	"github.com/knative/pkg/apis"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"

	v1alpha1 "github.com/knative/build/pkg/apis/build/v1alpha1"
	"github.com/knative/build/pkg/credentials"
	"github.com/knative/build/pkg/credentials/dockercreds"
	"github.com/knative/build/pkg/credentials/gitcreds"
)

const workspaceDir = "/workspace"

// These are effectively const, but Go doesn't have such an annotation.
var (
	emptyVolumeSource = corev1.VolumeSource{
		EmptyDir: &corev1.EmptyDirVolumeSource{},
	}
	// These are injected into all of the source/step containers.
	implicitEnvVars = []corev1.EnvVar{{
		Name:  "HOME",
		Value: "/builder/home",
	}}
	implicitVolumeMounts = []corev1.VolumeMount{{
		Name:      "workspace",
		MountPath: workspaceDir,
	}, {
		Name:      "home",
		MountPath: "/builder/home",
	}}
	implicitVolumes = []corev1.Volume{{
		Name:         "workspace",
		VolumeSource: emptyVolumeSource,
	}, {
		Name:         "home",
		VolumeSource: emptyVolumeSource,
	}}
)

const (
	// Prefixes to add to the name of the init containers.
	// IMPORTANT: Changing these values without changing fluentd collection configuration
	// will break log collection for init containers.
	initContainerPrefix        = "build-step-"
	unnamedInitContainerPrefix = "build-step-unnamed-"
	// A label with the following is added to the pod to identify the pods belonging to a build.
	buildNameLabelKey = "build.knative.dev/buildName"
	// Name of the credential initialization container.
	credsInit = "credential-initializer"
	// Names for source containers.
	gitSource    = "git-source"
	gcsSource    = "gcs-source"
	customSource = "custom-source"
)

var (
	// The container used to initialize credentials before the build runs.
	credsImage = flag.String("creds-image", "override-with-creds:latest",
		"The container image for preparing our Build's credentials.")
	// The container with Git that we use to implement the Git source step.
	gitImage = flag.String("git-image", "override-with-git:latest",
		"The container image containing our Git binary.")
	// The container that just prints build successful.
	nopImage = flag.String("nop-image", "override-with-nop:latest",
		"The container image run at the end of the build to log build success")
	gcsFetcherImage = flag.String("gcs-fetcher-image", "gcr.io/cloud-builders/gcs-fetcher:latest",
		"The container image containing our GCS fetcher binary.")
)

// TODO(mattmoor): Should we move this somewhere common, because of the flag?
func gitToContainer(source v1alpha1.SourceSpec, index int) (*corev1.Container, error) {
	git := source.Git
	if git.Url == "" {
		return nil, apis.ErrMissingField("b.spec.source.git.url")
	}
	if git.Revision == "" {
		return nil, apis.ErrMissingField("b.spec.source.git.revision")
	}

	args := []string{"-url", git.Url,
		"-revision", git.Revision,
	}

	if source.TargetPath != "" {
		args = append(args, []string{"-path", source.TargetPath}...)
	}

	containerName := initContainerPrefix + gitSource + "-"

	// update container name to suffix source name
	if source.Name != "" {
		containerName = containerName + source.Name
	} else {
		containerName = containerName + strconv.Itoa(index)
	}

	return &corev1.Container{
		Name:         containerName,
		Image:        *gitImage,
		Args:         args,
		VolumeMounts: implicitVolumeMounts,
		WorkingDir:   workspaceDir,
		Env:          implicitEnvVars,
	}, nil
}

func gcsToContainer(source v1alpha1.SourceSpec, index int) (*corev1.Container, error) {
	gcs := source.GCS
	if gcs.Location == "" {
		return nil, apis.ErrMissingField("b.spec.source.gcs.location")
	}
	args := []string{"--type", string(gcs.Type), "--location", gcs.Location}
	// dest_dir is the destination directory for GCS files to be copies"
	if source.TargetPath != "" {
		args = append(args, "--dest_dir", filepath.Join(workspaceDir, source.TargetPath))
	}

	// source name is empty then use `build-step-gcs-source` name
	containerName := initContainerPrefix + gcsSource + "-"

	// update container name to include `name` as suffix
	if source.Name != "" {
		containerName = containerName + source.Name
	} else {
		containerName = containerName + strconv.Itoa(index)
	}

	return &corev1.Container{
		Name:         containerName,
		Image:        *gcsFetcherImage,
		Args:         args,
		VolumeMounts: implicitVolumeMounts,
		WorkingDir:   workspaceDir,
		Env:          implicitEnvVars,
	}, nil
}

func customToContainer(source *corev1.Container, name string) (*corev1.Container, error) {
	if source.Name != "" {
		return nil, apis.ErrMissingField("b.spec.source.name")
	}
	custom := source.DeepCopy()

	// source name is empty then use `custom-source` name
	if name == "" {
		name = customSource
	} else {
		name = customSource + "-" + name
	}
	custom.Name = name
	return custom, nil
}

func makeCredentialInitializer(build *v1alpha1.Build, kubeclient kubernetes.Interface) (*corev1.Container, []corev1.Volume, error) {
	serviceAccountName := build.Spec.ServiceAccountName
	if serviceAccountName == "" {
		serviceAccountName = "default"
	}

	sa, err := kubeclient.CoreV1().ServiceAccounts(build.Namespace).Get(serviceAccountName, metav1.GetOptions{})
	if err != nil {
		return nil, nil, err
	}

	builders := []credentials.Builder{dockercreds.NewBuilder(), gitcreds.NewBuilder()}

	// Collect the volume declarations, there mounts into the cred-init container, and the arguments to it.
	volumes := []corev1.Volume{}
	volumeMounts := implicitVolumeMounts
	args := []string{}
	for _, secretEntry := range sa.Secrets {
		secret, err := kubeclient.CoreV1().Secrets(build.Namespace).Get(secretEntry.Name, metav1.GetOptions{})
		if err != nil {
			return nil, nil, err
		}

		matched := false
		for _, b := range builders {
			if sa := b.MatchingAnnotations(secret); len(sa) > 0 {
				matched = true
				args = append(args, sa...)
			}
		}

		if matched {
			name := fmt.Sprintf("secret-volume-%s", secret.Name)
			volumeMounts = append(volumeMounts, corev1.VolumeMount{
				Name:      name,
				MountPath: credentials.VolumeName(secret.Name),
			})
			volumes = append(volumes, corev1.Volume{
				Name: name,
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: secret.Name,
					},
				},
			})
		}
	}

	return &corev1.Container{
		Name:         initContainerPrefix + credsInit,
		Image:        *credsImage,
		Args:         args,
		VolumeMounts: volumeMounts,
		Env:          implicitEnvVars,
		WorkingDir:   workspaceDir,
	}, volumes, nil
}

// MakePod converts a Build object to a Pod which implements the build specified
// by the supplied CRD.
func MakePod(build *v1alpha1.Build, kubeclient kubernetes.Interface) (*corev1.Pod, error) {
	build = build.DeepCopy()

	cred, secrets, err := makeCredentialInitializer(build, kubeclient)
	if err != nil {
		return nil, err
	}

	initContainers := []corev1.Container{*cred}
	var sources []v1alpha1.SourceSpec
	// if source is present convert into sources
	if source := build.Spec.Source; source != nil {
		sources = []v1alpha1.SourceSpec{*source}
	}
	for _, source := range build.Spec.Sources {
		sources = append(sources, source)
	}
	workspaceSubPath := ""

	for i, source := range sources {
		switch {
		case source.Git != nil:
			git, err := gitToContainer(source, i)
			if err != nil {
				return nil, err
			}
			initContainers = append(initContainers, *git)
		case source.GCS != nil:
			gcs, err := gcsToContainer(source, i)
			if err != nil {
				return nil, err
			}
			initContainers = append(initContainers, *gcs)
		case source.Custom != nil:
			cust, err := customToContainer(source.Custom, source.Name)
			if err != nil {
				return nil, err
			}
			// Prepend the custom container to the steps, to be augmented later with env, volume mounts, etc.
			build.Spec.Steps = append([]corev1.Container{*cust}, build.Spec.Steps...)
		}
		// webhook validation checks that only one source has subPath defined
		workspaceSubPath = source.SubPath
	}

	for i, step := range build.Spec.Steps {
		step.Env = append(implicitEnvVars, step.Env...)
		// TODO(mattmoor): Check that volumeMounts match volumes.

		// Add implicit volume mounts, unless the user has requested
		// their own volume mount at that path.
		requestedVolumeMounts := map[string]bool{}
		for _, vm := range step.VolumeMounts {
			requestedVolumeMounts[filepath.Clean(vm.MountPath)] = true
		}
		for _, imp := range implicitVolumeMounts {
			if !requestedVolumeMounts[filepath.Clean(imp.MountPath)] {
				// If the build's source specifies a subpath,
				// use that in the implicit workspace volume
				// mount.
				if workspaceSubPath != "" && imp.Name == "workspace" {
					imp.SubPath = workspaceSubPath
				}
				step.VolumeMounts = append(step.VolumeMounts, imp)
			}
		}

		if step.WorkingDir == "" {
			step.WorkingDir = workspaceDir
		}
		if step.Name == "" {
			step.Name = fmt.Sprintf("%v%d", unnamedInitContainerPrefix, i)
		} else {
			step.Name = fmt.Sprintf("%v%v", initContainerPrefix, step.Name)
		}

		initContainers = append(initContainers, step)
	}
	// Add our implicit volumes and any volumes needed for secrets to the explicitly
	// declared user volumes.
	volumes := append(build.Spec.Volumes, implicitVolumes...)
	volumes = append(volumes, secrets...)
	if err := v1alpha1.ValidateVolumes(volumes); err != nil {
		return nil, err
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			// We execute the build's pod in the same namespace as where the build was
			// created so that it can access colocated resources.
			Namespace: build.Namespace,
			// Ensure our Pod gets a unique name.
			GenerateName: fmt.Sprintf("%s-", build.Name),
			// If our parent Build is deleted, then we should be as well.
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(build, schema.GroupVersionKind{
					Group:   v1alpha1.SchemeGroupVersion.Group,
					Version: v1alpha1.SchemeGroupVersion.Version,
					Kind:    "Build",
				}),
			},
			Annotations: map[string]string{
				"sidecar.istio.io/inject": "false",
			},
			Labels: map[string]string{
				buildNameLabelKey: build.Name,
			},
		},
		Spec: corev1.PodSpec{
			// If the build fails, don't restart it.
			RestartPolicy:  corev1.RestartPolicyNever,
			InitContainers: initContainers,
			Containers: []corev1.Container{{
				Name:  "nop",
				Image: *nopImage,
			}},
			ServiceAccountName: build.Spec.ServiceAccountName,
			Volumes:            volumes,
			NodeSelector:       build.Spec.NodeSelector,
			Affinity:           build.Spec.Affinity,
		},
	}, nil
}
