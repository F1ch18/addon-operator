package module_manager

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	log2 "github.com/flant/addon-operator/pkg/log"
	"github.com/flant/shell-operator/pkg/utils/manifest"
	"github.com/kennygrant/sanitize"
	log "github.com/sirupsen/logrus"
	uuid "gopkg.in/satori/go.uuid.v1"

	. "github.com/flant/addon-operator/pkg/hook/types"
	. "github.com/flant/shell-operator/pkg/hook/binding_context"
	. "github.com/flant/shell-operator/pkg/hook/types"

	sh_app "github.com/flant/shell-operator/pkg/app"
	"github.com/flant/shell-operator/pkg/executor"
	utils_file "github.com/flant/shell-operator/pkg/utils/file"

	"github.com/flant/addon-operator/pkg/app"
	"github.com/flant/addon-operator/pkg/helm"
	"github.com/flant/addon-operator/pkg/utils"
)

type Module struct {
	Name string
	Path string
	// module values from modules/values.yaml file
	CommonStaticConfig *utils.ModuleConfig
	// module values from modules/<module name>/values.yaml
	StaticConfig *utils.ModuleConfig

	LastReleaseManifests []manifest.Manifest

	moduleManager *moduleManager
}

func NewModule(name, path string) *Module {
	return &Module{
		Name: name,
		Path: path,
	}
}

func (m *Module) WithModuleManager(moduleManager *moduleManager) {
	m.moduleManager = moduleManager
}

func (m *Module) SafeName() string {
	return sanitize.BaseName(m.Name)
}

// Run is a phase of module lifecycle that runs onStartup and beforeHelm hooks, helm upgrade --install command and afterHelm hook.
// It is a handler of task MODULE_RUN
func (m *Module) Run(onStartup bool, logLabels map[string]string, afterStartupCb func() error) (bool, error) {
	logLabels = utils.MergeLabels(logLabels, map[string]string{
		"module": m.Name,
		"queue":  "main",
	})

	if err := m.cleanup(); err != nil {
		return false, err
	}

	// Hooks can delete release resources, so stop resources monitor before run hooks.
	m.moduleManager.HelmResourcesManager.StopMonitor(m.Name)

	if onStartup {
		if err := m.runHooksByBinding(OnStartup, logLabels); err != nil {
			return false, err
		}

		if err := afterStartupCb(); err != nil {
			return false, err
		}
	}

	var err error
	log2.MeasureTimeToLog(func() {
		err = m.runHooksByBinding(BeforeHelm, logLabels)
	}, "runHooksByBinding BeforeHelm", logLabels)
	if err != nil {
		return false, err
	}

	log2.MeasureTimeToLog(func() {
		err = m.runHelmInstall(logLabels)
	}, "runHelmInstall", logLabels)
	if err != nil {
		return false, err
	}

	//if err := m.runHelmInstall(logLabels); err != nil {
	//	return false, err
	//}

	var valuesChanged bool
	log2.MeasureTimeToLog(func() {
		valuesChanged, err = m.runHooksByBindingAndCheckValues(AfterHelm, logLabels)
	}, "runHooksByBinding AfterHelm", logLabels)
	if err != nil {
		return false, err
	}

	//valuesChanged, err := m.runHooksByBindingAndCheckValues(AfterHelm, logLabels)
	if err != nil {
		return false, err
	}
	// Do not send to mm.moduleValuesChanged, changed values are handled by TaskHandler.
	return valuesChanged, nil
}

// Delete removes helm release if it exists and runs afterDeleteHelm hooks.
// It is a handler for MODULE_DELETE task.
func (m *Module) Delete(logLabels map[string]string) error {
	deleteLogLabels := utils.MergeLabels(logLabels,
		map[string]string{
			"module": m.Name,
			"queue":  "main",
		})
	logEntry := log.WithFields(utils.LabelsToLogFields(deleteLogLabels))

	// Stop resources monitor before deleting release
	m.moduleManager.HelmResourcesManager.StopMonitor(m.Name)

	// Если есть chart, но нет релиза — warning
	// если нет чарта — молча перейти к хукам
	// если есть и chart и релиз — удалить
	chartExists, _ := m.checkHelmChart()
	if chartExists {
		releaseExists, err := helm.NewClient(deleteLogLabels).IsReleaseExists(m.generateHelmReleaseName())
		if !releaseExists {
			if err != nil {
				logEntry.Warnf("Cannot find helm release '%s' for module '%s'. Helm error: %s", m.generateHelmReleaseName(), m.Name, err)
			} else {
				logEntry.Warnf("Cannot find helm release '%s' for module '%s'.", m.generateHelmReleaseName(), m.Name)
			}
		} else {
			// Chart and release are existed, so run helm delete command
			err := helm.NewClient(deleteLogLabels).DeleteRelease(m.generateHelmReleaseName())
			if err != nil {
				return err
			}
		}
	}

	return m.runHooksByBinding(AfterDeleteHelm, deleteLogLabels)
}

func (m *Module) cleanup() error {
	chartExists, err := m.checkHelmChart()
	if !chartExists {
		if err != nil {
			log.Debugf("MODULE '%s': cleanup is not needed: %s", m.Name, err)
			return nil
		}
	}

	helmLogLabels := map[string]string{
		"module": m.Name,
	}

	if err := helm.NewClient(helmLogLabels).DeleteSingleFailedRevision(m.generateHelmReleaseName()); err != nil {
		return err
	}

	if err := helm.NewClient(helmLogLabels).DeleteOldFailedRevisions(m.generateHelmReleaseName()); err != nil {
		return err
	}

	return nil
}

func (m *Module) runHelmInstall(logLabels map[string]string) error {
	logEntry := log.WithFields(utils.LabelsToLogFields(logLabels))

	chartExists, err := m.checkHelmChart()
	if !chartExists {
		if err != nil {
			logEntry.Debugf("no Chart.yaml, helm is not needed: %s", err)
			return nil
		}
	}

	helmReleaseName := m.generateHelmReleaseName()

	//valuesPath, err := m.prepareValuesYamlFile()
	var valuesPath string
	//var err error
	log2.MeasureTimeToLog(func() {
		valuesPath, err = m.prepareValuesYamlFile()
	}, "prepareValuesYamlFile", logLabels)
	if err != nil {
		return err
	}

	// Render templates to prevent excess helm runs.
	helmClient := helm.NewClient(logLabels)
	//renderedManifests, err := helmClient.Render(m.Path, []string{valuesPath},
	//	[]string{},
	//	app.Namespace)
	var renderedManifests string
	log2.MeasureTimeToLog(func() {
		renderedManifests, err = helmClient.Render(m.Path, []string{valuesPath},
			[]string{},
			app.Namespace)
	}, "helm render", logLabels)
	if err != nil {
		return err
	}
	checksum := utils.CalculateStringsChecksum(renderedManifests)

	manifests, err := manifest.GetManifestListFromYamlDocuments(renderedManifests)
	if err != nil {
		return err
	}
	m.LastReleaseManifests = manifests

	// Skip upgrades if nothing is changes
	runUpgradeRelease, err := m.ShouldRunHelmUpgrade(helmClient, helmReleaseName, checksum, manifests, logLabels)
	if err != nil {
		return err
	}

	if !runUpgradeRelease {
		// Start resources monitor if release is not changed
		if !m.moduleManager.HelmResourcesManager.HasMonitor(m.Name) {
			log2.MeasureTimeToLog(func() {
				m.moduleManager.HelmResourcesManager.StartMonitor(m.Name, manifests, app.Namespace)
			}, "release is not changed, start monitor", logLabels)
		}
		return nil
	}

	log2.MeasureTimeToLog(func() {
		err = helmClient.UpgradeRelease(
			helmReleaseName,
			m.Path,
			[]string{valuesPath},
			[]string{fmt.Sprintf("_addonOperatorModuleChecksum=%s", checksum)},
			//helm.Client.TillerNamespace(),
			app.Namespace,
		)
	}, "helmClient.UpgradeRelease", logLabels)
	if err != nil {
		return err
	}
	// Start monitor resources if release was successful
	log2.MeasureTimeToLog(func() {
		m.moduleManager.HelmResourcesManager.StartMonitor(m.Name, manifests, app.Namespace)
	}, "Start monitor helm resources", logLabels)

	return nil
}

func (m *Module) ShouldRunHelmUpgrade(helmClient helm.HelmClient, releaseName string, checksum string, manifests []manifest.Manifest, logLabels map[string]string) (bool, error) {
	logEntry := log.WithFields(utils.LabelsToLogFields(logLabels))

	isReleaseExists, err := helmClient.IsReleaseExists(releaseName)
	if err != nil {
		return false, err
	}

	// Always run helm upgrade if there is no release
	if !isReleaseExists {
		logEntry.Debugf("helm release '%s' not exists: upgrade helm release", releaseName)
		return true, nil
	}

	_, status, err := helmClient.LastReleaseStatus(releaseName)
	if err != nil {
		return false, err
	}

	// Run helm upgrade if last release is failed
	if status == "FAILED" {
		logEntry.Debugf("helm release '%s' has FAILED status: upgrade helm release", releaseName)
		return true, nil
	}

	// Get values for a non failed release.
	releaseValues, err := helmClient.GetReleaseValues(releaseName)
	if err != nil {
		return false, err
	}

	// Run helm upgrade if there is no stored checksum
	recordedChecksum, hasKey := releaseValues["_addonOperatorModuleChecksum"]
	if !hasKey {
		logEntry.Debugf("helm release '%s' has no saved checksum of values: upgrade helm release", releaseName)
		return true, nil
	}

	// Calculate a checksum of current values and compare to a stored checksum.
	// Run helm upgrade if checksum is changed
	if recordedChecksumStr, ok := recordedChecksum.(string); ok {
		if recordedChecksumStr != checksum {
			logEntry.Debugf("helm release '%s' checksum '%s' is changed to '%s': upgrade helm release", releaseName, recordedChecksumStr, checksum)
			return true, nil
		}
	}

	// Check if there are absent resources
	absent, err := m.moduleManager.HelmResourcesManager.GetAbsentResources(manifests, app.Namespace)
	if err != nil {
		return false, err
	}

	// Run helm upgrade if there are absent resources
	if len(absent) > 0 {
		logEntry.Debugf("helm release '%s' has %d absent resources: upgrade helm release", releaseName, len(absent))
		return true, nil
	}

	logEntry.Debugf("helm release '%s': skip upgrade helm release", releaseName, checksum)
	return false, nil
}

// runHooksByBinding gets all hooks for binding, for each hook it creates a BindingContext,
// sets KubernetesSnapshots and runs the hook.
func (m *Module) runHooksByBinding(binding BindingType, logLabels map[string]string) error {
	moduleHooks := m.moduleManager.GetModuleHooksInOrder(m.Name, binding)

	for _, moduleHookName := range moduleHooks {
		moduleHook := m.moduleManager.GetModuleHook(moduleHookName)

		bc := BindingContext{
			Binding: ContextBindingType[binding],
		}

		log2.MeasureTimeToLog(func() {
			if binding == BeforeHelm || binding == AfterHelm || binding == AfterDeleteHelm {
				bc.Snapshots = moduleHook.HookController.KubernetesSnapshots()
				bc.Metadata.IncludeAllSnapshots = true
			}
		}, "Update KubernetesSnapshots", logLabels)
		// Update kubernetes snapshots just before execute a hook
		bc.Metadata.BindingType = binding

		//err := moduleHook.Run(binding, []BindingContext{bc}, logLabels)
		var err error
		log2.MeasureTimeToLog(func() {
			err = moduleHook.Run(binding, []BindingContext{bc}, logLabels)
		}, "moduleHook.Run", logLabels)
		if err != nil {
			return err
		}
	}

	return nil
}

// runHooksByBinding gets all hooks for binding, for each hook it creates a BindingContext,
// sets KubernetesSnapshots and runs the hook. If values are changed after hooks execution, return true.
func (m *Module) runHooksByBindingAndCheckValues(binding BindingType, logLabels map[string]string) (bool, error) {
	moduleHooks := m.moduleManager.GetModuleHooksInOrder(m.Name, binding)

	valuesChecksum, err := m.Values().Checksum()
	if err != nil {
		return false, err
	}

	for _, moduleHookName := range moduleHooks {
		moduleHook := m.moduleManager.GetModuleHook(moduleHookName)

		bc := BindingContext{
			Binding: ContextBindingType[binding],
		}
		log2.MeasureTimeToLog(func() {
			if binding == BeforeHelm || binding == AfterHelm || binding == AfterDeleteHelm {
				bc.Snapshots = moduleHook.HookController.KubernetesSnapshots()
				bc.Metadata.IncludeAllSnapshots = true
			}
		}, "Update KubernetesSnapshots", logLabels)

		//// Update kubernetes snapshots just before execute a hook
		//if binding == BeforeHelm || binding == AfterHelm || binding == AfterDeleteHelm {
		//	bc.Snapshots = moduleHook.HookController.KubernetesSnapshots()
		//	bc.Metadata.IncludeAllSnapshots = true
		//}
		bc.Metadata.BindingType = binding

		//err := moduleHook.Run(binding, []BindingContext{bc}, logLabels)
		var err error
		log2.MeasureTimeToLog(func() {
			err = moduleHook.Run(binding, []BindingContext{bc}, logLabels)
		}, "moduleHook.Run", logLabels)
		if err != nil {
			return false, err
		}
	}

	newValuesChecksum, err := m.Values().Checksum()
	if err != nil {
		return false, err
	}

	if newValuesChecksum != valuesChecksum {
		return true, nil
	}

	return false, nil
}

// CONFIG_VALUES_PATH
func (m *Module) prepareConfigValuesJsonFile() (string, error) {
	data, err := m.ConfigValues().JsonBytes()
	if err != nil {
		return "", err
	}

	path := filepath.Join(m.moduleManager.TempDir, fmt.Sprintf("%s.module-config-values-%s.json", m.SafeName(), uuid.NewV4().String()))
	err = dumpData(path, data)
	if err != nil {
		return "", err
	}

	log.Debugf("Prepared module %s config values:\n%s", m.Name, m.ConfigValues().DebugString())

	return path, nil
}

// values.yaml for helm
func (m *Module) prepareValuesYamlFile() (string, error) {
	data, err := m.Values().YamlBytes()
	if err != nil {
		return "", err
	}

	path := filepath.Join(m.moduleManager.TempDir, fmt.Sprintf("%s.module-values.yaml-%s", m.SafeName(), uuid.NewV4().String()))
	err = dumpData(path, data)
	if err != nil {
		return "", err
	}

	log.Debugf("Prepared module %s values:\n%s", m.Name, m.Values().DebugString())

	return path, nil
}

// VALUES_PATH
func (m *Module) prepareValuesJsonFileWith(values utils.Values) (string, error) {
	data, err := values.JsonBytes()
	if err != nil {
		return "", err
	}

	path := filepath.Join(m.moduleManager.TempDir, fmt.Sprintf("%s.module-values-%s.json", m.SafeName(), uuid.NewV4().String()))
	err = dumpData(path, data)
	if err != nil {
		return "", err
	}

	log.Debugf("Prepared module %s values:\n%s", m.Name, values.DebugString())

	return path, nil
}

func (m *Module) prepareValuesJsonFile() (string, error) {
	return m.prepareValuesJsonFileWith(m.Values())
}

func (m *Module) prepareValuesJsonFileForEnabledScript(precedingEnabledModules []string) (string, error) {
	return m.prepareValuesJsonFileWith(m.valuesForEnabledScript(precedingEnabledModules))
}

func (m *Module) checkHelmChart() (bool, error) {
	chartPath := filepath.Join(m.Path, "Chart.yaml")

	if _, err := os.Stat(chartPath); os.IsNotExist(err) {
		return false, fmt.Errorf("path '%s' is not found", chartPath)
	}
	return true, nil
}

// generateHelmReleaseName returns a string that can be used as a helm release name.
//
// TODO Now it returns just a module name. Should it be cleaned from special symbols?
func (m *Module) generateHelmReleaseName() string {
	return m.Name
}

// ConfigValues returns values from ConfigMap: global section and module section
func (m *Module) ConfigValues() utils.Values {
	return utils.MergeValues(
		// global section
		utils.Values{"global": map[string]interface{}{}},
		m.moduleManager.kubeGlobalConfigValues,
		// module section
		utils.Values{m.ValuesKey(): map[string]interface{}{}},
		m.moduleManager.kubeModulesConfigValues[m.Name],
	)
}

// constructValues returns effective values for module hook:
//
// global section: static + kube + patches from hooks
//
// module section: static + kube + patches from hooks
func (m *Module) constructValues() utils.Values {
	var err error

	res := utils.MergeValues(
		// global
		utils.Values{"global": map[string]interface{}{}},
		m.moduleManager.commonStaticValues.Global(),
		m.moduleManager.kubeGlobalConfigValues,
		// module
		utils.Values{m.ValuesKey(): map[string]interface{}{}},
		m.CommonStaticConfig.Values,
		m.StaticConfig.Values,
		m.moduleManager.kubeModulesConfigValues[m.Name],
	)

	for _, patches := range [][]utils.ValuesPatch{
		m.moduleManager.globalDynamicValuesPatches,
		m.moduleManager.modulesDynamicValuesPatches[m.Name],
	} {
		for _, patch := range patches {
			// Invariant: do not store patches that does not apply
			// Give user error for patches early, after patch receive

			res, _, err = utils.ApplyValuesPatch(res, patch)
			if err != nil {
				panic(err)
			}
		}
	}

	return res
}

// valuesForEnabledScript returns merged values for enabled script.
// There is enabledModules key in global section with previously enabled modules.
func (m *Module) valuesForEnabledScript(precedingEnabledModules []string) utils.Values {
	res := m.constructValues()
	res = utils.MergeValues(res, utils.Values{
		"global": map[string]interface{}{
			"enabledModules": precedingEnabledModules,
		},
	})
	return res
}

// values returns merged values for hooks.
// There is enabledModules key in global section with all enabled modules.
func (m *Module) Values() utils.Values {
	res := m.constructValues()
	res = utils.MergeValues(res, utils.Values{
		"global": map[string]interface{}{
			"enabledModules": m.moduleManager.enabledModulesInOrder,
		},
	})
	return res
}

func (m *Module) ValuesKey() string {
	return utils.ModuleNameToValuesKey(m.Name)
}

func (m *Module) prepareModuleEnabledResultFile() (string, error) {
	path := filepath.Join(m.moduleManager.TempDir, fmt.Sprintf("%s.module-enabled-result", m.Name))
	if err := CreateEmptyWritableFile(path); err != nil {
		return "", err
	}
	return path, nil
}

func (m *Module) readModuleEnabledResult(filePath string) (bool, error) {
	data, err := ioutil.ReadFile(filePath)
	if err != nil {
		return false, fmt.Errorf("cannot read %s: %s", filePath, err)
	}

	value := strings.TrimSpace(string(data))

	if value == "true" {
		return true, nil
	} else if value == "false" {
		return false, nil
	}

	return false, fmt.Errorf("expected 'true' or 'false', got '%s'", value)
}

func (m *Module) checkIsEnabledByScript(precedingEnabledModules []string, logLabels map[string]string) (bool, error) {
	logEntry := log.WithFields(utils.LabelsToLogFields(logLabels))
	enabledScriptPath := filepath.Join(m.Path, "enabled")

	f, err := os.Stat(enabledScriptPath)
	if os.IsNotExist(err) {
		logEntry.Debugf("MODULE '%s' is ENABLED. Enabled script is not exist!", m.Name)
		return true, nil
	} else if err != nil {
		logEntry.Errorf("Cannot stat enabled script '%s': %s", enabledScriptPath, err)
		return false, err
	}

	if !utils_file.IsFileExecutable(f) {
		logEntry.Errorf("Found non-executable enabled script '%s'", enabledScriptPath)
		return false, fmt.Errorf("non-executable enable script")
	}

	// ValuesLock.Lock()
	configValuesPath, err := m.prepareConfigValuesJsonFile()
	if err != nil {
		logEntry.Errorf("Prepare CONFIG_VALUES_PATH file for '%s': %s", enabledScriptPath, err)
		return false, err
	}
	defer func() {
		if sh_app.DebugKeepTmpFiles == "yes" {
			return
		}
		err := os.Remove(configValuesPath)
		if err != nil {
			log.WithField("module", m.Name).
				Errorf("Remove tmp file '%s': %s", configValuesPath, err)
		}
	}()

	valuesPath, err := m.prepareValuesJsonFileForEnabledScript(precedingEnabledModules)
	if err != nil {
		logEntry.Errorf("Prepare VALUES_PATH file for '%s': %s", enabledScriptPath, err)
		return false, err
	}
	defer func() {
		if sh_app.DebugKeepTmpFiles == "yes" {
			return
		}
		err := os.Remove(valuesPath)
		if err != nil {
			log.WithField("module", m.Name).
				Errorf("Remove tmp file '%s': %s", configValuesPath, err)
		}
	}()

	enabledResultFilePath, err := m.prepareModuleEnabledResultFile()
	if err != nil {
		logEntry.Errorf("Prepare MODULE_ENABLED_RESULT file for '%s': %s", enabledScriptPath, err)
		return false, err
	}
	defer func() {
		if sh_app.DebugKeepTmpFiles == "yes" {
			return
		}
		err := os.Remove(enabledResultFilePath)
		if err != nil {
			log.WithField("module", m.Name).
				Errorf("Remove tmp file '%s': %s", configValuesPath, err)
		}
	}()

	logEntry.Debugf("Execute enabled script '%s', preceding modules: %v", enabledScriptPath, precedingEnabledModules)

	// ValuesLock.UnLock()

	envs := make([]string, 0)
	envs = append(envs, os.Environ()...)
	envs = append(envs, fmt.Sprintf("CONFIG_VALUES_PATH=%s", configValuesPath))
	envs = append(envs, fmt.Sprintf("VALUES_PATH=%s", valuesPath))
	envs = append(envs, fmt.Sprintf("MODULE_ENABLED_RESULT=%s", enabledResultFilePath))

	cmd := executor.MakeCommand("", enabledScriptPath, []string{}, envs)

	if err := executor.RunAndLogLines(cmd, logLabels); err != nil {
		logEntry.Errorf("Fail to run enabled script '%s': %s", enabledScriptPath, err)
		return false, err
	}

	moduleEnabled, err := m.readModuleEnabledResult(enabledResultFilePath)
	if err != nil {
		logEntry.Errorf("Read enabled result from '%s': %s", enabledScriptPath, err)
		return false, fmt.Errorf("bad enabled result")
	}

	result := "Disabled"
	if moduleEnabled {
		result = "Enabled"
	}
	logEntry.Infof("Enabled script run successful, result: module %q", result)
	return moduleEnabled, nil
}

var ValidModuleNameRe = regexp.MustCompile(`^[0-9][0-9][0-9]-(.*)$`)

func SearchModules(modulesDir string) (modules []*Module, err error) {
	files, err := ioutil.ReadDir(modulesDir) // returns a list of modules sorted by filename
	if err != nil {
		return nil, fmt.Errorf("list modules directory '%s': %s", modulesDir, err)
	}

	badModulesDirs := make([]string, 0)
	modules = make([]*Module, 0)

	for _, file := range files {
		if !file.IsDir() {
			continue
		}
		matchRes := ValidModuleNameRe.FindStringSubmatch(file.Name())
		if matchRes != nil {
			moduleName := matchRes[1]
			modulePath := filepath.Join(modulesDir, file.Name())
			module := NewModule(moduleName, modulePath)
			modules = append(modules, module)
		} else {
			badModulesDirs = append(badModulesDirs, filepath.Join(modulesDir, file.Name()))
		}
	}

	if len(badModulesDirs) > 0 {
		return nil, fmt.Errorf("modules directory contains directories not matched ValidModuleRegex '%s': %s", ValidModuleNameRe, strings.Join(badModulesDirs, ", "))
	}

	return
}

// RegisterModules load all available modules from modules directory
// FIXME: Only 000-name modules are loaded, allow non-prefixed modules.
func (mm *moduleManager) RegisterModules() error {
	log.Debug("Search and register modules")

	modules, err := SearchModules(mm.ModulesDir)
	if err != nil {
		return err
	}
	log.Debugf("Found %d modules", len(modules))

	// load global and modules common static values from modules/values.yaml
	if err := mm.loadCommonStaticValues(); err != nil {
		return fmt.Errorf("load common values for modules: %s", err)
	}

	for _, module := range modules {
		logEntry := log.WithField("module", module.Name)

		module.WithModuleManager(mm)

		// load static config from values.yaml
		err := module.loadStaticValues()
		if err != nil {
			logEntry.Errorf("Load values.yaml: %s", err)
			return fmt.Errorf("bad module values")
		}

		mm.allModulesByName[module.Name] = module
		mm.allModulesNamesInOrder = append(mm.allModulesNamesInOrder, module.Name)

		logEntry.Infof("Module is registered")
	}

	return nil
}

// loadStaticValues loads config for module from values.yaml
// Module is enabled if values.yaml is not exists.
func (m *Module) loadStaticValues() (err error) {
	m.CommonStaticConfig, err = utils.NewModuleConfig(m.Name).LoadFromValues(m.moduleManager.commonStaticValues)
	if err != nil {
		return err
	}
	log.Debugf("module %s common static values: %s", m.Name, m.CommonStaticConfig.Values.DebugString())

	valuesYamlPath := filepath.Join(m.Path, "values.yaml")

	if _, err := os.Stat(valuesYamlPath); os.IsNotExist(err) {
		m.StaticConfig = utils.NewModuleConfig(m.Name)
		log.Debugf("module %s is static disabled: no values.yaml exists", m.Name)
		return nil
	}

	data, err := ioutil.ReadFile(valuesYamlPath)
	if err != nil {
		return fmt.Errorf("cannot read '%s': %s", m.Path, err)
	}

	m.StaticConfig, err = utils.NewModuleConfig(m.Name).FromYaml(data)
	if err != nil {
		return err
	}
	log.Debugf("module %s static values: %s", m.Name, m.StaticConfig.Values.DebugString())
	return nil
}

func (mm *moduleManager) loadCommonStaticValues() error {
	valuesPath := filepath.Join(mm.ModulesDir, "values.yaml")
	if _, err := os.Stat(valuesPath); os.IsNotExist(err) {
		log.Debugf("No common static values file: %s", err)
		return nil
	}

	valuesYaml, err := ioutil.ReadFile(valuesPath)
	if err != nil {
		return fmt.Errorf("load common values file '%s': %s", valuesPath, err)
	}

	values, err := utils.NewValuesFromBytes(valuesYaml)
	if err != nil {
		return err
	}

	mm.commonStaticValues = values

	log.Debugf("Successfully load common static values:\n%s", mm.commonStaticValues.DebugString())

	return nil
}

func dumpData(filePath string, data []byte) error {
	err := ioutil.WriteFile(filePath, data, 0644)
	if err != nil {
		return err
	}
	return nil
}
