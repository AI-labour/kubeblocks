/*
Copyright (C) 2022-2025 ApeCloud Co., Ltd

This file is part of KubeBlocks project

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package configmanager

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"

	"github.com/imdario/mergo"
	"gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	appsv1 "github.com/apecloud/kubeblocks/apis/apps/v1"
	parametersv1alpha1 "github.com/apecloud/kubeblocks/apis/parameters/v1alpha1"
	"github.com/apecloud/kubeblocks/pkg/configuration/core"
	cfgutil "github.com/apecloud/kubeblocks/pkg/configuration/util"
	"github.com/apecloud/kubeblocks/pkg/constant"
	"github.com/apecloud/kubeblocks/pkg/controller/multicluster"
	viper "github.com/apecloud/kubeblocks/pkg/viperx"
)

const (
	configTemplateName = "reload.yaml"
	scriptVolumePrefix = "cm-script-"
	configVolumePrefix = "cm-config-"

	scriptConfigField    = "scripts"
	formatterConfigField = "formatterConfig"

	configManagerConfigVolumeName = "config-manager-config"
	configManagerConfig           = "config-manager.yaml"
	configManagerConfigMountPoint = "/opt/config-manager"
	configManagerCMPrefix         = "sidecar-"
)

const (
	KBScriptVolumePath = "/opt/kb-tools/reload"
	KBConfigVolumePath = "/opt/kb-tools/config"

	KBTOOLSScriptsPathEnv  = "TOOLS_SCRIPTS_PATH"
	KBConfigManagerPathEnv = "TOOLS_PATH"
)

func BuildConfigManagerContainerParams(cli client.Client, ctx context.Context, managerParams *CfgManagerBuildParams, volumeDirs []corev1.VolumeMount) error {
	var volume *corev1.VolumeMount
	var buildParam *ConfigSpecMeta

	allVolumeMounts := getWatchedVolume(volumeDirs, managerParams.ConfigSpecsBuildParams)
	for i := range managerParams.ConfigSpecsBuildParams {
		buildParam = &managerParams.ConfigSpecsBuildParams[i]
		volume = FindVolumeMount(managerParams.Volumes, buildParam.ConfigSpec.VolumeName)
		if volume == nil {
			ctrl.Log.Info(fmt.Sprintf("volume mount not be use : %s", buildParam.ConfigSpec.VolumeName))
			continue
		}
		buildParam.MountPoint = volume.MountPath
		if err := buildConfigSpecHandleMeta(cli, ctx, buildParam, managerParams); err != nil {
			return err
		}
	}
	downwardAPIVolumes := buildDownwardAPIVolumes(managerParams)
	allVolumeMounts = append(allVolumeMounts, downwardAPIVolumes...)
	managerParams.Volumes = append(managerParams.Volumes, downwardAPIVolumes...)
	return buildConfigManagerArgs(managerParams, allVolumeMounts, cli, ctx)
}

func getWatchedVolume(volumeDirs []corev1.VolumeMount, buildParams []ConfigSpecMeta) []corev1.VolumeMount {
	enableWatchVolume := func(volume corev1.VolumeMount) bool {
		for _, param := range buildParams {
			if param.ConfigSpec.VolumeName != volume.Name {
				continue
			}
			switch param.ReloadType {
			case parametersv1alpha1.TPLScriptType:
				return core.IsWatchModuleForTplTrigger(param.ReloadAction.TPLScriptTrigger)
			case parametersv1alpha1.ShellType:
				return core.IsWatchModuleForShellTrigger(param.ReloadAction.ShellTrigger)
			default:
				return true
			}
		}
		return false
	}

	allVolumeMounts := make([]corev1.VolumeMount, 0, len(volumeDirs))
	for _, volume := range volumeDirs {
		if enableWatchVolume(volume) {
			allVolumeMounts = append(allVolumeMounts, volume)
		}
	}
	return allVolumeMounts
}

func buildDownwardAPIVolumes(params *CfgManagerBuildParams) []corev1.VolumeMount {
	for _, buildParam := range params.ConfigSpecsBuildParams {
		for _, info := range buildParam.DownwardAPIOptions {
			if FindVolumeMount(params.DownwardAPIVolumes, info.Name) == nil {
				buildDownwardAPIVolume(params, info)
			}
		}
	}
	return params.DownwardAPIVolumes
}

func buildConfigManagerArgs(params *CfgManagerBuildParams, volumeDirs []corev1.VolumeMount, cli client.Client, ctx context.Context) error {
	args := buildConfigManagerCommonArgs(volumeDirs)
	args = append(args, "--operator-update-enable")
	args = append(args, "--tcp", strconv.Itoa(int(params.ContainerPort)))

	if err := createOrUpdateConfigMap(fromConfigSpecMeta(params.ConfigSpecsBuildParams), params, cli, ctx); err != nil {
		return err
	}
	args = append(args, "--config", filepath.Join(configManagerConfigMountPoint, configManagerConfig))
	params.Args = args
	return nil
}

func buildCMForConfig(manager *CfgManagerBuildParams, cmKey client.ObjectKey, config string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmKey.Name,
			Namespace: cmKey.Namespace,
			Labels: map[string]string{
				constant.AppInstanceLabelKey:    manager.Cluster.Name,
				constant.AppManagedByLabelKey:   constant.AppName,
				constant.KBAppComponentLabelKey: manager.ComponentName,
			},
		},
		Data: map[string]string{
			configManagerConfig: config,
		},
	}
}

func createOrUpdateConfigMap(configInfo []ConfigSpecInfo, manager *CfgManagerBuildParams, cli client.Client, ctx context.Context) error {
	createConfigCM := func(configKey client.ObjectKey, config string) error {
		scheme, err := appsv1.SchemeBuilder.Build()
		if err != nil {
			return err
		}
		cmObj := buildCMForConfig(manager, configKey, config)
		if err := controllerutil.SetOwnerReference(manager.Cluster, cmObj, scheme); err != nil {
			return err
		}
		return cli.Create(ctx, cmObj, inDataContext())
	}
	updateConfigCM := func(cm *corev1.ConfigMap, newConfig string) error {
		patch := client.MergeFrom(cm.DeepCopy())
		cm.Data[configManagerConfig] = newConfig
		return cli.Patch(ctx, cm, patch, inDataContext())
	}

	config, err := cfgutil.ToYamlConfig(configInfo)
	if err != nil {
		return err
	}
	cmObj := &corev1.ConfigMap{}
	cmKey := client.ObjectKey{
		Namespace: manager.Cluster.GetNamespace(),
		Name:      fmt.Sprintf("%s%s-%s-config-manager-config", configManagerCMPrefix, manager.Cluster.GetName(), manager.ComponentName),
	}
	err = cli.Get(ctx, cmKey, cmObj, inDataContext())
	switch {
	default:
		return err
	case err == nil:
		err = updateConfigCM(cmObj, string(config))
	case apierrors.IsNotFound(err):
		err = createConfigCM(cmKey, string(config))
	}
	if err == nil {
		buildReloadScriptVolume(cmKey.Name, manager, configManagerConfigMountPoint, configManagerConfigVolumeName)
	}
	return err
}

func fromConfigSpecMeta(metas []ConfigSpecMeta) []ConfigSpecInfo {
	configSpecs := make([]ConfigSpecInfo, 0, len(metas))
	for _, meta := range metas {
		configSpecs = append(configSpecs, meta.ConfigSpecInfo)
	}
	return configSpecs
}

func FindVolumeMount(volumeDirs []corev1.VolumeMount, volumeName string) *corev1.VolumeMount {
	for i := range volumeDirs {
		if volumeDirs[i].Name == volumeName {
			return &volumeDirs[i]
		}
	}
	return nil
}

func buildConfigSpecHandleMeta(cli client.Client, ctx context.Context, buildParam *ConfigSpecMeta, cmBuildParam *CfgManagerBuildParams) error {
	for _, script := range buildParam.ScriptConfig {
		if err := buildCfgManagerScripts(script, cmBuildParam, cli, ctx, buildParam.ConfigSpec); err != nil {
			return err
		}
	}
	if buildParam.ReloadType == parametersv1alpha1.TPLScriptType {
		return buildTPLScriptCM(buildParam, cmBuildParam, cli, ctx)
	}
	return nil
}

func buildTPLScriptCM(configSpecBuildMeta *ConfigSpecMeta, manager *CfgManagerBuildParams, cli client.Client, ctx context.Context) error {
	var (
		options      = configSpecBuildMeta.TPLScriptTrigger
		formatConfig = configSpecBuildMeta.FormatterConfig
		mountPoint   = GetScriptsMountPoint(configSpecBuildMeta.ConfigSpec)
	)

	reloadYamlFn := func(cm *corev1.ConfigMap) error {
		newData, err := checkAndUpdateReloadYaml(cm.Data, configTemplateName, formatConfig)
		if err != nil {
			return err
		}
		cm.Data = newData
		return nil
	}

	referenceCMKey := client.ObjectKey{
		Namespace: options.Namespace,
		Name:      options.ScriptConfigMapRef,
	}
	scriptCMKey := client.ObjectKey{
		Namespace: manager.Cluster.GetNamespace(),
		Name:      fmt.Sprintf("%s%s-%s", configManagerCMPrefix, options.ScriptConfigMapRef, manager.Cluster.GetName()),
	}
	if err := checkOrCreateConfigMap(referenceCMKey, scriptCMKey, cli, ctx, manager.Cluster, reloadYamlFn); err != nil {
		return err
	}
	buildReloadScriptVolume(scriptCMKey.Name, manager, mountPoint, GetScriptsVolumeName(configSpecBuildMeta.ConfigSpec))
	configSpecBuildMeta.TPLConfig = filepath.Join(mountPoint, configTemplateName)
	return nil
}

func buildDownwardAPIVolume(manager *CfgManagerBuildParams, fieldInfo parametersv1alpha1.DownwardAPIChangeTriggeredAction) {
	manager.DownwardAPIVolumes = append(manager.DownwardAPIVolumes, corev1.VolumeMount{
		Name:      fieldInfo.Name,
		MountPath: fieldInfo.MountPoint,
	})
	manager.CMConfigVolumes = append(manager.CMConfigVolumes, corev1.Volume{
		Name: fieldInfo.Name,
		VolumeSource: corev1.VolumeSource{
			DownwardAPI: &corev1.DownwardAPIVolumeSource{
				Items: fieldInfo.Items,
			}},
	})
}

func buildReloadScriptVolume(scriptCMName string, manager *CfgManagerBuildParams, mountPoint, volumeName string) {
	var execMode int32 = 0755
	manager.Volumes = append(manager.Volumes, corev1.VolumeMount{
		Name:      volumeName,
		MountPath: mountPoint,
	})
	manager.ScriptVolume = append(manager.ScriptVolume, corev1.Volume{
		Name: volumeName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: scriptCMName},
				DefaultMode:          &execMode,
			},
		},
	})
}

func checkOrCreateConfigMap(referenceCM client.ObjectKey, scriptCMKey client.ObjectKey, cli client.Client, ctx context.Context, cluster *appsv1.Cluster, fn func(cm *corev1.ConfigMap) error) error {
	var (
		err   error
		op    controllerutil.OperationResult
		refCM = corev1.ConfigMap{}
	)

	if err = cli.Get(ctx, referenceCM, &refCM); err != nil {
		return err
	}

	expected, err := createScripts(&refCM, scriptCMKey, cluster, fn)
	if err != nil {
		return err
	}
	existing := expected.DeepCopy()
	mutateFn := func() (err error) {
		mergeWithOverride(existing.Labels, expected.Labels)
		mergeWithOverride(existing.Annotations, expected.Annotations)
		existing.SetOwnerReferences(expected.GetOwnerReferences())
		existing.Data = expected.Data
		return
	}

	updateLog := func() {
		ctrl.Log.Info(fmt.Sprintf("expected cm object[%s] has been %s", scriptCMKey, op))
	}

	defer updateLog()
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		result, createOrUpdateErr := ctrl.CreateOrUpdate(ctx, cli, existing, mutateFn)
		op = result
		return createOrUpdateErr
	})
}

func createScripts(refCM *corev1.ConfigMap, key client.ObjectKey, owner client.Object, fn func(cm *corev1.ConfigMap) error) (*corev1.ConfigMap, error) {
	expected := &corev1.ConfigMap{}

	scheme, _ := appsv1.SchemeBuilder.Build()
	expected.Data = refCM.Data
	if fn != nil {
		if err := fn(expected); err != nil {
			return nil, err
		}
	}
	expected.SetLabels(refCM.GetLabels())
	expected.SetName(key.Name)
	expected.SetNamespace(key.Namespace)
	expected.SetAnnotations(refCM.GetAnnotations())
	if err := controllerutil.SetOwnerReference(owner, expected, scheme); err != nil {
		return nil, err
	}
	return expected, nil
}

func mergeWithOverride(dst, src interface{}) {
	_ = mergo.Merge(dst, src, mergo.WithOverride)
}

func checkAndUpdateReloadYaml(data map[string]string, reloadConfig string, formatterConfig parametersv1alpha1.FileFormatConfig) (map[string]string, error) {
	configObject := make(map[string]interface{})
	if content, ok := data[reloadConfig]; ok {
		if err := yaml.Unmarshal([]byte(content), &configObject); err != nil {
			return nil, err
		}
	}
	if res, _, _ := unstructured.NestedFieldNoCopy(configObject, scriptConfigField); res == nil {
		return nil, core.MakeError("reload.yaml required field: %s", scriptConfigField)
	}

	formatObject, err := apiruntime.DefaultUnstructuredConverter.ToUnstructured(&formatterConfig)
	if err != nil {
		return nil, err
	}
	if err := unstructured.SetNestedField(configObject, formatObject, formatterConfigField); err != nil {
		return nil, err
	}
	b, err := yaml.Marshal(configObject)
	if err != nil {
		return nil, err
	}
	data[reloadConfig] = string(b)
	return data, nil
}

func buildCfgManagerScripts(options parametersv1alpha1.ScriptConfig, manager *CfgManagerBuildParams, cli client.Client, ctx context.Context, configSpec appsv1.ComponentFileTemplate) error {
	mountPoint := filepath.Join(KBScriptVolumePath, configSpec.Name)
	referenceCMKey := client.ObjectKey{
		Namespace: options.Namespace,
		Name:      options.ScriptConfigMapRef,
	}
	scriptsCMKey := client.ObjectKey{
		Namespace: manager.Cluster.GetNamespace(),
		Name:      fmt.Sprintf("%s%s-%s", configManagerCMPrefix, options.ScriptConfigMapRef, manager.Cluster.GetName()),
	}
	if err := checkOrCreateConfigMap(referenceCMKey, scriptsCMKey, cli, ctx, manager.Cluster, nil); err != nil {
		return err
	}
	buildReloadScriptVolume(scriptsCMKey.Name, manager, mountPoint, GetScriptsVolumeName(configSpec))
	return nil
}

func GetScriptsMountPoint(configSpec appsv1.ComponentFileTemplate) string {
	return filepath.Join(KBScriptVolumePath, configSpec.Name)
}

func GetScriptsVolumeName(configSpec appsv1.ComponentFileTemplate) string {
	return fmt.Sprintf("%s%s", scriptVolumePrefix, configSpec.Name)
}

func buildConfigManagerCommonArgs(volumeDirs []corev1.VolumeMount) []string {
	args := make([]string, 0)
	// set grpc port
	// args = append(args, "--tcp", viper.GetString(cfgcore.ConfigManagerGPRCPortEnv))
	args = append(args, "--log-level", viper.GetString(constant.ConfigManagerLogLevel))
	for _, volume := range volumeDirs {
		args = append(args, "--volume-dir", volume.MountPath)
	}
	return args
}

func inDataContext() *multicluster.ClientOption {
	return multicluster.InDataContext()
}
