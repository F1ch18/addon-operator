package utils

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"reflect"
	"sort"
	"strings"

	"github.com/davecgh/go-spew/spew"
	log "github.com/sirupsen/logrus"

	"github.com/evanphx/json-patch"
	"github.com/peterbourgon/mergemap"
	"github.com/segmentio/go-camelcase"
	"gopkg.in/yaml.v3"
	k8syaml "sigs.k8s.io/yaml"

	utils_checksum "github.com/flant/shell-operator/pkg/utils/checksum"
)

const (
	GlobalValuesKey = "global"
)

type ValuesPatchType string

const ConfigMapPatch ValuesPatchType = "CONFIG_MAP_PATCH"
const MemoryValuesPatch ValuesPatchType = "MEMORY_VALUES_PATCH"

// Values stores values for modules or hooks by name.
type Values map[string]interface{}

type ValuesPatch struct {
	Operations []*ValuesPatchOperation
}

func (p *ValuesPatch) ToJsonPatch() (jsonpatch.Patch, error) {
	data, err := json.Marshal(p.Operations)
	if err != nil {
		return nil, err
	}
	patch, err := jsonpatch.DecodePatch(data)
	if err != nil {
		return nil, err
	}
	return patch, nil
}

// Apply calls jsonpatch.Apply to mutate a JSON document according to the patch.
func (p *ValuesPatch) Apply(doc []byte) ([]byte, error) {
	patch, err := p.ToJsonPatch()
	if err != nil {
		return nil, err
	}
	return patch.Apply(doc)
}

type ValuesPatchOperation struct {
	Op    string      `json:"op"`
	Path  string      `json:"path"`
	Value interface{} `json:"value,omitempty"`
}

func (op *ValuesPatchOperation) ToString() string {
	data, err := json.Marshal(op.Value)
	if err != nil {
		// This should not happen, because ValuesPatchOperation is created with Unmarshal!
		return fmt.Sprintf("{\"op\":\"%s\", \"path\":\"%s\", \"value-error\": \"%s\" }", op.Op, op.Path, err)
	}
	return string(data)
}

// ModuleNameToValuesKey returns camelCased name from kebab-cased (very-simple-module become verySimpleModule)
func ModuleNameToValuesKey(moduleName string) string {
	return camelcase.Camelcase(moduleName)
}

// ModuleNameFromValuesKey returns kebab-cased name from camelCased (verySimpleModule become ver-simple-module)
func ModuleNameFromValuesKey(moduleValuesKey string) string {
	b := make([]byte, 0, 64)
	l := len(moduleValuesKey)
	i := 0

	for i < l {
		c := moduleValuesKey[i]

		if c >= 'A' && c <= 'Z' {
			if i > 0 {
				// Appends dash module name parts delimiter.
				b = append(b, '-')
			}
			// Appends lowercased symbol.
			b = append(b, c+('a'-'A'))
		} else if c >= '0' && c <= '9' {
			if i > 0 {
				// Appends dash module name parts delimiter.
				b = append(b, '-')
			}
			b = append(b, c)
		} else {
			b = append(b, c)
		}

		i++
	}

	return string(b)
}

// NewValuesFromBytes loads values sections from maps in yaml or json format
func NewValuesFromBytes(data []byte) (Values, error) {
	var values map[string]interface{}

	err := k8syaml.Unmarshal(data, &values)
	if err != nil {
		return nil, fmt.Errorf("bad values data: %s\n%s", err, string(data))
	}

	return Values(values), nil
}

// NewValues load all sections from input data and makes sure that input map
// can be marshaled to yaml and that yaml is compatible with json.
func NewValues(data map[string]interface{}) (Values, error) {
	yamlDoc, err := k8syaml.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("data is not compatible with JSON and YAML: %s, data:\n%s", err, spew.Sdump(data))
	}

	var values Values
	if err := k8syaml.Unmarshal(yamlDoc, &values); err != nil {
		return nil, fmt.Errorf("convert data YAML to values: %s, data:\n%s", err, spew.Sdump(data))
	}

	return values, nil
}

// NewGlobalValues creates Values with global section loaded from input string.
func NewGlobalValues(globalSectionContent string) (Values, error) {
	var section map[string]interface{}
	if err := k8syaml.Unmarshal([]byte(globalSectionContent), &section); err != nil {
		return nil, fmt.Errorf("global section is not compatible with JSON and YAML: %s, data:\n%s", err, globalSectionContent)
	}

	return Values(map[string]interface{}{
		GlobalValuesKey: section,
	}), nil
}

// TODO used only in tests
func MustValuesPatch(res *ValuesPatch, err error) *ValuesPatch {
	if err != nil {
		panic(err)
	}
	return res
}

func JsonPatchFromReader(r io.Reader) (jsonpatch.Patch, error) {
	var operations = make([]jsonpatch.Operation, 0)

	dec := json.NewDecoder(r)
	for {
		var jsonStreamItem interface{}
		if err := dec.Decode(&jsonStreamItem); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}

		switch v := jsonStreamItem.(type) {
		case []interface{}:
			for _, item := range v {
				operation, err := DecodeJsonPatchOperation(item)
				if err != nil {
					return nil, err
				}
				operations = append(operations, operation)
			}
		case map[string]interface{}:
			operation, err := DecodeJsonPatchOperation(v)
			if err != nil {
				return nil, err
			}
			operations = append(operations, operation)
		}
	}

	return jsonpatch.Patch(operations), nil
}

func JsonPatchFromBytes(data []byte) (jsonpatch.Patch, error) {
	return JsonPatchFromReader(bytes.NewReader(data))
}
func JsonPatchFromString(in string) (jsonpatch.Patch, error) {
	return JsonPatchFromReader(strings.NewReader(in))
}

func DecodeJsonPatchOperation(v interface{}) (jsonpatch.Operation, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal operation to bytes: %s", err)
	}

	var res jsonpatch.Operation
	err = json.Unmarshal(data, &res)
	if err != nil {
		return nil, fmt.Errorf("unmarshal operation from bytes: %s", err)
	}
	return res, nil
}

// ValuesPatchFromBytes reads a JSON stream of json patches
// and single operations from bytes and returns a ValuesPatch with
// all json patch operations.
// TODO do we need a separate ValuesPatchOperation type??
func ValuesPatchFromBytes(data []byte) (*ValuesPatch, error) {
	// Get combined patch from bytes
	patch, err := JsonPatchFromBytes(data)
	if err != nil {
		return nil, fmt.Errorf("bad json-patch data: %s\n%s", err, string(data))
	}

	// Convert combined patch to bytes
	combined, err := json.Marshal(patch)
	if err != nil {
		return nil, fmt.Errorf("json patch marshal: %s\n%s", err, string(data))
	}

	var operations []*ValuesPatchOperation
	if err := json.Unmarshal(combined, &operations); err != nil {
		return nil, fmt.Errorf("values patch operations: %s\n%s", err, string(data))
	}

	return &ValuesPatch{Operations: operations}, nil
}

func ValuesPatchFromFile(filePath string) (*ValuesPatch, error) {
	data, err := ioutil.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %s", filePath, err)
	}

	if len(data) == 0 {
		return nil, nil
	}

	return ValuesPatchFromBytes(data)
}

func AppendValuesPatch(valuesPatches []ValuesPatch, newValuesPatch ValuesPatch) []ValuesPatch {
	return CompactValuesPatches(valuesPatches, newValuesPatch)
}

func CompactValuesPatches(valuesPatches []ValuesPatch, newValuesPatch ValuesPatch) []ValuesPatch {
	operations := []*ValuesPatchOperation{}

	for _, patch := range valuesPatches {
		operations = append(operations, patch.Operations...)
	}
	operations = append(operations, newValuesPatch.Operations...)

	return []ValuesPatch{CompactPatches(operations)}
}

// CompactPatches simplifies a patches tree — one path, one operation.
func CompactPatches(operations []*ValuesPatchOperation) ValuesPatch {
	patchesTree := make(map[string][]*ValuesPatchOperation)

	for _, op := range operations {
		// remove previous operations for subpaths if got 'remove' operation for parent path
		if op.Op == "remove" {
			for subPath := range patchesTree {
				if len(op.Path) < len(subPath) && strings.HasPrefix(subPath, op.Path+"/") {
					delete(patchesTree, subPath)
				}
			}
		}

		if _, ok := patchesTree[op.Path]; !ok {
			patchesTree[op.Path] = make([]*ValuesPatchOperation, 0)
		}

		// 'add' can be squashed to only one operation
		if op.Op == "add" {
			patchesTree[op.Path] = []*ValuesPatchOperation{op}
		}

		// 'remove' is squashed to 'remove' and 'add' for future Apply calls
		if op.Op == "remove" {
			// find most recent 'add' operation
			hasPreviousAdd := false
			for _, prevOp := range patchesTree[op.Path] {
				if prevOp.Op == "add" {
					patchesTree[op.Path] = []*ValuesPatchOperation{prevOp, op}
					hasPreviousAdd = true
				}
			}

			if !hasPreviousAdd {
				// Something bad happens — a sequence contains a 'remove' operation without previous 'add' operation
				// Append virtual 'add' operation to not fail future Apply calls.
				patchesTree[op.Path] = []*ValuesPatchOperation{
					{
						Op:    "add",
						Path:  op.Path,
						Value: "guard-patch-for-successful-remove",
					},
					op,
				}
			}
		}
	}

	// Sort paths for proper 'add' sequence
	paths := []string{}
	for path := range patchesTree {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	newOps := []*ValuesPatchOperation{}
	for _, path := range paths {
		newOps = append(newOps, patchesTree[path]...)
	}

	newValuesPatch := ValuesPatch{Operations: newOps}
	return newValuesPatch
}

// ApplyValuesPatch applies a set of json patch operations to the values and returns a result
func ApplyValuesPatch(values Values, valuesPatch ValuesPatch) (Values, bool, error) {
	var err error

	jsonDoc, err := json.Marshal(values)
	if err != nil {
		return nil, false, err
	}

	resJsonDoc, err := valuesPatch.Apply(jsonDoc)
	if err != nil {
		return nil, false, err
	}

	resValues := make(Values)
	if err = json.Unmarshal(resJsonDoc, &resValues); err != nil {
		return nil, false, err
	}

	valuesChanged := !reflect.DeepEqual(values, resValues)

	return resValues, valuesChanged, nil
}

func ValidateHookValuesPatch(valuesPatch ValuesPatch, acceptableKey string) error {
	for _, op := range valuesPatch.Operations {
		if op.Op == "replace" {
			return fmt.Errorf("unsupported patch operation '%s': '%s'", op.Op, op.ToString())
		}

		pathParts := strings.Split(op.Path, "/")
		if len(pathParts) > 1 {
			affectedKey := pathParts[1]
			// patches for *Enabled keys are accepted from global hooks
			if strings.HasSuffix(affectedKey, "Enabled") && acceptableKey == GlobalValuesKey {
				continue
			}
			// patches for acceptableKey are allowed
			if affectedKey == acceptableKey {
				continue
			}
			// all other patches are denied
			return fmt.Errorf("unacceptable patch operation for path '%s' (only '%s' accepted): '%s'", affectedKey, acceptableKey, op.ToString())
		}
	}

	return nil
}

func FilterValuesPatch(valuesPatch ValuesPatch, rootPath string) ValuesPatch {
	resOps := []*ValuesPatchOperation{}

	for _, op := range valuesPatch.Operations {
		pathParts := strings.Split(op.Path, "/")
		if len(pathParts) > 1 {
			// patches for acceptableKey are allowed
			if pathParts[1] == rootPath {
				resOps = append(resOps, op)
			}
		}
	}

	newValuesPatch := ValuesPatch{Operations: resOps}
	return newValuesPatch
}

func EnabledFromValuesPatch(valuesPatch ValuesPatch) ValuesPatch {
	resOps := []*ValuesPatchOperation{}

	for _, op := range valuesPatch.Operations {
		pathParts := strings.Split(op.Path, "/")
		if len(pathParts) > 1 {
			// patches for acceptableKey are allowed
			if strings.HasSuffix(pathParts[1], "Enabled") {
				resOps = append(resOps, op)
			}
		}
	}

	newValuesPatch := ValuesPatch{Operations: resOps}
	return newValuesPatch
}

func MergeValues(values ...Values) Values {
	res := make(Values)

	for _, v := range values {
		res = mergemap.Merge(res, v)
	}

	return res
}

// DebugString returns values as yaml or an error line if dump is failed
func (v Values) DebugString() string {
	b, err := v.YamlBytes()
	if err != nil {
		return "bad values: " + err.Error()
	}
	return string(b)
}

func (v Values) Checksum() (string, error) {
	valuesJson, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return utils_checksum.CalculateChecksum(string(valuesJson)), nil
}

func (v Values) HasKey(key string) bool {
	_, has := v[key]
	return has
}

func (v Values) HasGlobal() bool {
	_, has := v[GlobalValuesKey]
	return has
}

func (v Values) Global() Values {
	globalValues, has := v[GlobalValuesKey]
	if has {
		data := map[string]interface{}{GlobalValuesKey: globalValues}
		newV, err := NewValues(data)
		if err != nil {
			log.Errorf("get global Values: %s", err)
		}
		return newV
	}
	return make(Values)
}

func (v Values) SectionByKey(key string) Values {
	sectionValues, has := v[key]
	if has {
		data := map[string]interface{}{key: sectionValues}
		newV, err := NewValues(data)
		if err != nil {
			log.Errorf("get section '%s' Values: %s", key, err)
		}
		return newV
	}
	return make(Values)
}

func (v Values) AsBytes(format string) ([]byte, error) {
	switch format {
	case "json":
		return json.Marshal(v)
	case "yaml":
		fallthrough
	default:
		return yaml.Marshal(v)
	}
}

func (v Values) AsString(format string) (string, error) {
	b, err := v.AsBytes(format)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// AsConfigMapData returns values as map that can be used as a 'data' field in the ConfigMap.
func (v Values) AsConfigMapData() (map[string]string, error) {
	res := make(map[string]string)

	for k, value := range v {
		dump, err := yaml.Marshal(value)
		if err != nil {
			return nil, err
		}
		res[k] = string(dump)
	}
	return res, nil
}

func (v Values) JsonString() (string, error) {
	return v.AsString("json")
}

func (v Values) JsonBytes() ([]byte, error) {
	return v.AsBytes("json")
}

func (v Values) YamlString() (string, error) {
	return v.AsString("yaml")
}

func (v Values) YamlBytes() ([]byte, error) {
	return v.AsBytes("yaml")
}

type ValuesLoader interface {
	Read() (Values, error)
}

type ValuesDumper interface {
	Write(values Values) error
}

// Load values by specific key from loader
func Load(key string, loader ValuesLoader) (Values, error) {
	return nil, nil
}

// LoadAll loads values from all keys from loader
func LoadAll(loader ValuesLoader) (Values, error) {
	return nil, nil
}

func Dump(values Values, dumper ValuesDumper) error {
	return nil
}

type ValuesDumperToJsonFile struct {
	FileName string
}

func NewDumperToJsonFile(path string) ValuesDumper {
	return &ValuesDumperToJsonFile{
		FileName: path,
	}
}

func (*ValuesDumperToJsonFile) Write(values Values) error {
	return fmt.Errorf("implement Write in ValuesDumperToJsonFile")
}

type ValuesLoaderFromJsonFile struct {
	FileName string
}

func NewLoaderFromJsonFile(path string) ValuesLoader {
	return &ValuesLoaderFromJsonFile{
		FileName: path,
	}
}

func (*ValuesLoaderFromJsonFile) Read() (Values, error) {
	return nil, fmt.Errorf("implement Read methoid")
}
