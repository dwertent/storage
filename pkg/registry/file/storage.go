package file

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"

	"github.com/kubescape/go-logger"
	"github.com/kubescape/go-logger/helpers"
	"github.com/kubescape/storage/pkg/apis/softwarecomposition"
	"github.com/olvrng/ujson"
	"github.com/spf13/afero"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/conversion"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/apiserver/pkg/storage"
)

const (
	jsonExt            = ".json"
	metadataExt        = ".metadata"
	DefaultStorageRoot = "/data"
)

type objState struct {
	obj  runtime.Object
	meta *storage.ResponseMeta
	rev  int64
	data []byte
}

// StorageImpl offers a common interface for object marshaling/unmarshaling operations and
// hides all the storage-related operations behind it.
type StorageImpl struct {
	appFs           afero.Fs
	watchDispatcher watchDispatcher
	lock            sync.RWMutex
	root            string
	versioner       storage.Versioner
}

var _ storage.Interface = &StorageImpl{}

func NewStorageImpl(appFs afero.Fs, root string) storage.Interface {
	return &StorageImpl{
		appFs:           appFs,
		watchDispatcher: newWatchDispatcher(),
		root:            root,
		versioner:       storage.APIObjectVersioner{},
	}
}

// Versioner Returns Versioner associated with this interface.
func (s *StorageImpl) Versioner() storage.Versioner {
	return s.versioner
}

func removeSpec(in []byte) ([]byte, error) {
	var out []byte
	err := ujson.Walk(in, func(_ int, key, value []byte) bool {
		if len(key) != 0 {
			if bytes.EqualFold(key, []byte(`"spec"`)) {
				// remove the key and value from the output
				return false
			}
		}
		// write to output
		if len(out) != 0 && ujson.ShouldAddComma(value, out[len(out)-1]) {
			out = append(out, ',')
		}
		if len(key) > 0 {
			out = append(out, key...)
			out = append(out, ':')
		}
		out = append(out, value...)
		return true
	})
	return out, err
}

func (s *StorageImpl) writeFiles(ctx context.Context, key string, obj runtime.Object, out runtime.Object) error {
	// do not alter obj
	dup := obj.DeepCopyObject()
	// set resourceversion
	if version, _ := s.versioner.ObjectResourceVersion(dup); version == 0 {
		if err := s.versioner.UpdateObject(dup, 1); err != nil {
			return fmt.Errorf("set resourceVersion failed: %v", err)
		}
	}
	// prepare path
	p := filepath.Join(s.root, key)
	_, spanLock := otel.Tracer("").Start(ctx, "waiting for lock")
	s.lock.Lock()
	spanLock.End()
	defer s.lock.Unlock()
	if err := s.appFs.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return err
	}
	// prepare json content
	jsonBytes, err := json.MarshalIndent(dup, "", "  ")
	if err != nil {
		return err
	}
	// prepare metadata content
	metadataBytes, err := removeSpec(jsonBytes)
	if err != nil {
		return err
	}
	// write json file
	err = afero.WriteFile(s.appFs, p+jsonExt, jsonBytes, 0644)
	if err != nil {
		return err // maybe not exit here to try writing metadata file
	}
	// write metadata file
	err = afero.WriteFile(s.appFs, p+metadataExt, metadataBytes, 0644)
	if err != nil {
		return err
	}
	// eventually fill out
	if out != nil {
		err = json.Unmarshal(jsonBytes, out)
		if err != nil {
			return err
		}
	}
	return nil
}

// Create adds a new object at a key even when it already exists. 'ttl' is time-to-live
// in seconds (and is ignored). If no error is returned and out is not nil, out will be
// set to the read value from database.
func (s *StorageImpl) Create(ctx context.Context, key string, obj, out runtime.Object, _ uint64) error {
	ctx, span := otel.Tracer("").Start(ctx, "StorageImpl.Create")
	span.SetAttributes(attribute.String("key", key))
	defer span.End()
	// resourceversion should not be set on create
	if version, err := s.versioner.ObjectResourceVersion(obj); err == nil && version != 0 {
		msg := "resourceVersion should not be set on objects to be created"
		logger.L().Ctx(ctx).Error(msg)
		return errors.New(msg)
	}
	// write files
	if err := s.writeFiles(ctx, key, obj, out); err != nil {
		logger.L().Ctx(ctx).Error("write files failed", helpers.Error(err), helpers.String("key", key))
		return err
	}

	// publish event to watchers
	s.watchDispatcher.Added(key, obj)
	return nil
}

// Delete removes the specified key and returns the value that existed at that spot.
// If key didn't exist, it will return NotFound storage error.
// If 'cachedExistingObject' is non-nil, it can be used as a suggestion about the
// current version of the object to avoid read operation from storage to get it.
// However, the implementations have to retry in case suggestion is stale.
func (s *StorageImpl) Delete(ctx context.Context, key string, out runtime.Object, _ *storage.Preconditions, _ storage.ValidateObjectFunc, _ runtime.Object) error {
	ctx, span := otel.Tracer("").Start(ctx, "StorageImpl.Delete")
	span.SetAttributes(attribute.String("key", key))
	defer span.End()
	p := filepath.Join(s.root, key)
	_, spanLock := otel.Tracer("").Start(ctx, "waiting for lock")
	s.lock.Lock()
	spanLock.End()
	defer s.lock.Unlock()
	// read json file
	b, err := afero.ReadFile(s.appFs, p+jsonExt)
	if err != nil {
		if errors.Is(err, afero.ErrFileNotFound) {
			return storage.NewKeyNotFoundError(key, 0)
		}
		logger.L().Ctx(ctx).Error("read file failed", helpers.Error(err), helpers.String("key", key))
		return err
	}
	// delete json and metadata files
	err = s.appFs.Remove(p + jsonExt)
	if err != nil {
		logger.L().Ctx(ctx).Error("remove json file failed", helpers.Error(err), helpers.String("key", key))
	}
	err = s.appFs.Remove(p + metadataExt)
	if err != nil {
		logger.L().Ctx(ctx).Error("remove metadata file failed", helpers.Error(err), helpers.String("key", key))
	}
	// try to fill out
	err = json.Unmarshal(b, out)
	if err != nil {
		logger.L().Ctx(ctx).Error("json unmarshal failed", helpers.Error(err), helpers.String("key", key))
		return err
	}

	// publish event to watchers
	s.watchDispatcher.Deleted(key, out)
	return nil
}

// Watch begins watching the specified key. Events are decoded into API objects,
// and any items selected by 'p' are sent down to returned watch.Interface.
// resourceVersion may be used to specify what version to begin watching,
// which should be the current resourceVersion, and no longer rv+1
// (e.g. reconnecting without missing any updates).
// If resource version is "0", this interface will get current object at given key
// and send it in an "ADDED" event, before watch starts.
func (s *StorageImpl) Watch(ctx context.Context, key string, _ storage.ListOptions) (watch.Interface, error) {
	_, span := otel.Tracer("").Start(ctx, "StorageImpl.Watch")
	span.SetAttributes(attribute.String("key", key))
	defer span.End()
	newWatcher := newWatcher(make(chan watch.Event))
	s.watchDispatcher.Register(key, newWatcher)
	return newWatcher, nil
}

// Get unmarshals object found at key into objPtr. On a not found error, will either
// return a zero object of the requested type, or an error, depending on 'opts.ignoreNotFound'.
// Treats empty responses and nil response nodes exactly like a not found error.
// The returned contents may be delayed, but it is guaranteed that they will
// match 'opts.ResourceVersion' according 'opts.ResourceVersionMatch'.
func (s *StorageImpl) Get(ctx context.Context, key string, opts storage.GetOptions, objPtr runtime.Object) error {
	ctx, span := otel.Tracer("").Start(ctx, "StorageImpl.Get")
	span.SetAttributes(attribute.String("key", key))
	defer span.End()
	p := filepath.Join(s.root, key)
	_, spanLock := otel.Tracer("").Start(ctx, "waiting for lock")
	s.lock.RLock()
	spanLock.End()
	defer s.lock.RUnlock()
	b, err := afero.ReadFile(s.appFs, p+jsonExt)
	if err != nil {
		if errors.Is(err, afero.ErrFileNotFound) {
			if opts.IgnoreNotFound {
				return runtime.SetZeroValue(objPtr)
			} else {
				return storage.NewKeyNotFoundError(key, 0)
			}
		}
		logger.L().Ctx(ctx).Error("read file failed", helpers.Error(err), helpers.String("key", key))
		return err
	}
	err = json.Unmarshal(b, objPtr)
	if err != nil {
		logger.L().Ctx(ctx).Error("json unmarshal failed", helpers.Error(err), helpers.String("key", key))
		return err
	}
	return nil
}

// GetList unmarshalls objects found at key into a *List api object (an object
// that satisfies runtime.IsList definition).
// If 'opts.Recursive' is false, 'key' is used as an exact match. If `opts.Recursive'
// is true, 'key' is used as a prefix.
// The returned contents may be delayed, but it is guaranteed that they will
// match 'opts.ResourceVersion' according 'opts.ResourceVersionMatch'.
func (s *StorageImpl) GetList(ctx context.Context, key string, _ storage.ListOptions, listObj runtime.Object) error {
	ctx, span := otel.Tracer("").Start(ctx, "StorageImpl.GetList")
	span.SetAttributes(attribute.String("key", key))
	defer span.End()
	listPtr, err := meta.GetItemsPtr(listObj)
	if err != nil {
		logger.L().Ctx(ctx).Error("get items ptr failed", helpers.Error(err), helpers.String("key", key))
		return err
	}
	v, err := conversion.EnforcePtr(listPtr)
	if err != nil || v.Kind() != reflect.Slice {
		logger.L().Ctx(ctx).Error("need ptr to slice", helpers.Error(err), helpers.String("key", key))
		return fmt.Errorf("need ptr to slice: %v", err)
	}
	p := filepath.Join(s.root, key)
	var files []string
	_, spanLock := otel.Tracer("").Start(ctx, "waiting for lock")
	s.lock.RLock()
	spanLock.End()
	if exists, _ := afero.Exists(s.appFs, p+metadataExt); exists {
		files = append(files, p+metadataExt)
	} else {
		_ = afero.Walk(s.appFs, p, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if !info.IsDir() && strings.HasSuffix(path, metadataExt) {
				files = append(files, path)
			}
			return nil
		})
	}
	s.lock.RUnlock()
	for _, path := range files {
		// we need to read the whole file
		b, err := afero.ReadFile(s.appFs, path)
		if err != nil {
			// skip if file is not readable, maybe it was deleted
			continue
		}
		// unmarshal into object
		elem := v.Type().Elem()
		obj := reflect.New(elem).Interface().(runtime.Object)
		err = json.Unmarshal(b, obj)
		if err != nil {
			logger.L().Ctx(ctx).Error("unmarshal file failed", helpers.Error(err), helpers.String("path", path))
			continue
		}
		// append to list
		v.Set(reflect.Append(v, reflect.ValueOf(obj).Elem()))
	}
	return nil
}

func (s *StorageImpl) getStateFromObject(ctx context.Context, obj runtime.Object) (*objState, error) {
	state := &objState{
		obj:  obj,
		meta: &storage.ResponseMeta{},
	}

	rv, err := s.versioner.ObjectResourceVersion(obj)
	if err != nil {
		logger.L().Ctx(ctx).Error("get object resource version failed", helpers.Error(err), helpers.Interface("object", obj))
		return nil, fmt.Errorf("couldn't get resource version: %v", err)
	}
	state.rev = int64(rv)
	state.meta.ResourceVersion = uint64(state.rev)

	state.data, err = json.Marshal(obj)
	if err != nil {
		logger.L().Ctx(ctx).Error("marshal object failed", helpers.Error(err), helpers.Interface("object", obj))
		return nil, err
	}
	if err := s.versioner.UpdateObject(state.obj, rv); err != nil {
		logger.L().Ctx(ctx).Error("update object version failed", helpers.Error(err), helpers.Interface("object", obj))
	}
	return state, nil
}

// GuaranteedUpdate keeps calling 'tryUpdate()' to update key 'key' (of type 'destination')
// retrying the update until success if there is index conflict.
// Note that object passed to tryUpdate may change across invocations of tryUpdate() if
// other writers are simultaneously updating it, so tryUpdate() needs to take into account
// the current contents of the object when deciding how the update object should look.
// If the key doesn't exist, it will return NotFound storage error if ignoreNotFound=false
// else `destination` will be set to the zero value of it's type.
// If the eventual successful invocation of `tryUpdate` returns an output with the same serialized
// contents as the input, it won't perform any update, but instead set `destination` to an object with those
// contents.
// If 'cachedExistingObject' is non-nil, it can be used as a suggestion about the
// current version of the object to avoid read operation from storage to get it.
// However, the implementations have to retry in case suggestion is stale.
//
// Example:
//
// s := /* implementation of Interface */
// err := s.GuaranteedUpdate(
//
//	 "myKey", &MyType{}, true, preconditions,
//	 func(input runtime.Object, res ResponseMeta) (runtime.Object, *uint64, error) {
//	   // Before each invocation of the user defined function, "input" is reset to
//	   // current contents for "myKey" in database.
//	   curr := input.(*MyType)  // Guaranteed to succeed.
//
//	   // Make the modification
//	   curr.Counter++
//
//	   // Return the modified object - return an error to stop iterating. Return
//	   // a uint64 to alter the TTL on the object, or nil to keep it the same value.
//	   return cur, nil, nil
//	}, cachedExistingObject
//
// )
func (s *StorageImpl) GuaranteedUpdate(
	ctx context.Context, key string, destination runtime.Object, ignoreNotFound bool,
	preconditions *storage.Preconditions, tryUpdate storage.UpdateFunc, cachedExistingObject runtime.Object) error {
	ctx, span := otel.Tracer("").Start(ctx, "StorageImpl.GuaranteedUpdate")
	span.SetAttributes(attribute.String("key", key))
	defer span.End()

	// key preparation is skipped
	// otel span tracking is skipped

	v, err := conversion.EnforcePtr(destination)
	if err != nil {
		logger.L().Ctx(ctx).Error("unable to convert output object to pointer", helpers.Error(err), helpers.String("key", key))
		return fmt.Errorf("unable to convert output object to pointer: %v", err)
	}

	getCurrentState := func() (*objState, error) {
		objPtr := reflect.New(v.Type()).Interface().(runtime.Object)
		err := s.Get(ctx, key, storage.GetOptions{IgnoreNotFound: ignoreNotFound}, objPtr)
		if err != nil {
			logger.L().Ctx(ctx).Error("get failed", helpers.Error(err), helpers.String("key", key))
			return nil, err
		}
		return s.getStateFromObject(ctx, objPtr)
	}

	var origState *objState
	var origStateIsCurrent bool
	if cachedExistingObject != nil {
		origState, err = s.getStateFromObject(ctx, cachedExistingObject)
	} else {
		origState, err = getCurrentState()
		origStateIsCurrent = true
	}
	if err != nil {
		logger.L().Ctx(ctx).Error("get original state failed", helpers.Error(err), helpers.String("key", key))
		return err
	}

	for {
		// run preconditions
		if err := preconditions.Check(key, origState.obj); err != nil {
			// If our data is already up-to-date, return the error
			if origStateIsCurrent {
				logger.L().Ctx(ctx).Error("preconditions check failed", helpers.Error(err), helpers.String("key", key))
				return err
			}

			// It's possible we were working with stale data
			// Actually fetch
			origState, err = getCurrentState()
			if err != nil {
				logger.L().Ctx(ctx).Error("get state failed", helpers.Error(err), helpers.String("key", key))
				return err
			}
			origStateIsCurrent = true
			// Retry
			continue
		}

		// run tryUpdate
		ret, _, err := tryUpdate(origState.obj, storage.ResponseMeta{})
		if err != nil {
			// If our data is already up-to-date, return the error
			if origStateIsCurrent {
				logger.L().Ctx(ctx).Error("tryUpdate func failed", helpers.Error(err), helpers.String("key", key))
				return err
			}

			// It's possible we were working with stale data
			// Remember the revision of the potentially stale data and the resulting update error
			cachedRev := origState.rev
			cachedUpdateErr := err

			// Actually fetch
			origState, err = getCurrentState()
			if err != nil {
				logger.L().Ctx(ctx).Error("get state failed", helpers.Error(err), helpers.String("key", key))
				return err
			}
			origStateIsCurrent = true

			// it turns out our cached data was not stale, return the error
			if cachedRev == origState.rev {
				logger.L().Ctx(ctx).Error("tryUpdate func failed", helpers.Error(err), helpers.String("key", key))
				return cachedUpdateErr
			}

			// Retry
			continue
		}

		// save to disk and fill into destination
		err = s.writeFiles(ctx, key, ret, destination)
		if err == nil {
			// Only successful updates should produce modification events
			s.watchDispatcher.Modified(key, ret)
		} else {
			logger.L().Ctx(ctx).Error("write files failed", helpers.Error(err), helpers.String("key", key))
		}
		return err
	}
}

// Count returns number of different entries under the key (generally being path prefix).
func (s *StorageImpl) Count(key string) (int64, error) {
	logger.L().Debug("Custom storage count", helpers.String("key", key))
	p := filepath.Join(s.root, key)
	s.lock.RLock()
	defer s.lock.RUnlock()
	if exists, _ := afero.Exists(s.appFs, p+jsonExt); exists {
		return 1, nil
	}
	n := 0
	err := afero.Walk(s.appFs, p, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(path, jsonExt) {
			n++
		}
		return nil
	})
	return int64(n), err
}

func (s *StorageImpl) getOneVulnManifestSummary(ctx context.Context, path string) (*softwarecomposition.VulnerabilityManifestSummary, error) {
	var vulnManifestSummary softwarecomposition.VulnerabilityManifestSummary

	b, err := afero.ReadFile(s.appFs, path)
	if err != nil {
		if errors.Is(err, afero.ErrFileNotFound) {
			return nil, storage.NewKeyNotFoundError(path, 0)
		}
		logger.L().Ctx(ctx).Error("read file failed", helpers.Error(err), helpers.String("key", path))
		return nil, err
	}
	err = json.Unmarshal(b, &vulnManifestSummary)
	if err != nil {
		logger.L().Ctx(ctx).Error("json unmarshal failed", helpers.Error(err), helpers.String("key", path))
		return nil, err
	}
	if _, exist := vulnManifestSummary.GetLabels()["kubescape.io/workload-namespace"]; !exist {
		return nil, storage.NewKeyNotFoundError(path, 0)
	}
	return &vulnManifestSummary, nil
}

func (s *StorageImpl) getVulnManifestSummaryDirPath(kind, namespace string) string {
	return filepath.Join(s.root, "spdx.softwarecomposition.kubescape.io", kind, namespace)
}

func (s *StorageImpl) aggregateVulnSummary(ctx context.Context, kind, namespace string) ([]softwarecomposition.VulnerabilityManifestSummary, []error) {
	var vsms []softwarecomposition.VulnerabilityManifestSummary
	var errs []error

	afero.Walk(s.appFs, s.getVulnManifestSummaryDirPath(kind, namespace), func(path string, info os.FileInfo, err error) error {
		if !strings.HasSuffix(path, jsonExt) {
			return nil
		}
		vulnManifestSummary, e := s.getOneVulnManifestSummary(ctx, path)
		if err != nil {
			errs = append(errs, e)
			return nil
		}
		vsms = append(vsms, *vulnManifestSummary)
		return nil
	})

	return vsms, errs
}

func (s *StorageImpl) GetByNamespace(ctx context.Context, kind, namespace string) ([]runtime.Object, error) {
	ctx, span := otel.Tracer("").Start(ctx, "GetByNamespace")
	span.SetAttributes(attribute.String("kind", kind), attribute.String("namespace", namespace))
	defer span.End()

	var objects []runtime.Object

	vsms, errs := s.aggregateVulnSummary(ctx, kind, namespace)
	for i := range errs {
		logger.L().Ctx(ctx).Warning("error per vuln Manifest file", helpers.Error(errs[i]), helpers.String("kind", kind), helpers.String("namespace", namespace))
	}

	if len(vsms) == 0 {
		return objects, storage.NewKeyNotFoundError(kind+"/"+namespace, 0)
	}

	for i := range vsms {
		obj := vsms[i].DeepCopyObject()
		objects = append(objects, obj)
	}

	return objects, nil
}

// currently we will not support it
func (s *StorageImpl) GetByCluster(ctx context.Context, kind string) ([]runtime.Object, error) {
	_, span := otel.Tracer("").Start(ctx, "GetByCluster")
	span.SetAttributes(attribute.String("kind", kind))
	defer span.End()

	var objects []runtime.Object

	return objects, storage.NewInvalidObjError("cluster", "")
}
