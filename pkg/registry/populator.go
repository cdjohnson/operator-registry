package registry

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/blang/semver"
	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/yaml"

	"github.com/operator-framework/operator-registry/pkg/image"
)

type Dependencies struct {
	RawMessage []map[string]interface{} `json:"dependencies" yaml:"dependencies"`
}

// DirectoryPopulator loads an unpacked operator bundle from a directory into the database.
type DirectoryPopulator struct {
	loader          Load
	graphLoader     GraphLoader
	querier         Query
	imageDirMap     map[image.Reference]string
	overwriteDirMap map[string]map[image.Reference]string
	overwrite       bool
}

func NewDirectoryPopulator(loader Load, graphLoader GraphLoader, querier Query, imageDirMap map[image.Reference]string, overwriteDirMap map[string]map[image.Reference]string, overwrite bool) *DirectoryPopulator {
	return &DirectoryPopulator{
		loader:          loader,
		graphLoader:     graphLoader,
		querier:         querier,
		imageDirMap:     imageDirMap,
		overwriteDirMap: overwriteDirMap,
		overwrite:       overwrite,
	}
}

func (i *DirectoryPopulator) Populate(mode Mode) error {
	var errs []error
	imagesToAdd := make([]*ImageInput, 0)
	for to, from := range i.imageDirMap {
		imageInput, err := NewImageInput(to, from)
		if err != nil {
			errs = append(errs, err)
			continue
		}

		imagesToAdd = append(imagesToAdd, imageInput)
	}

	imagesToReAdd := make([]*ImageInput, 0)
	for pkg := range i.overwriteDirMap {
		for to, from := range i.overwriteDirMap[pkg] {
			imageInput, err := NewImageInput(to, from)
			if err != nil {
				errs = append(errs, err)
				continue
			}

			imagesToReAdd = append(imagesToReAdd, imageInput)
		}
	}

	if len(errs) > 0 {
		return utilerrors.NewAggregate(errs)
	}

	err := i.loadManifests(imagesToAdd, imagesToReAdd, mode)
	if err != nil {
		return err
	}

	return nil
}

func (i *DirectoryPopulator) globalSanityCheck(imagesToAdd []*ImageInput) error {
	var errs []error
	images := make(map[string]struct{})
	for _, image := range imagesToAdd {
		images[image.Bundle.BundleImage] = struct{}{}
	}

	attemptedOverwritesPerPackage := map[string]struct{}{}
	for _, image := range imagesToAdd {
		validOverwrite := false
		bundlePaths, err := i.querier.GetBundlePathsForPackage(context.TODO(), image.Bundle.Package)
		if err != nil {
			// Assume that this means that the bundle is empty
			// Or that this is the first time the package is loaded.
			return nil
		}
		for _, bundlePath := range bundlePaths {
			if _, ok := images[bundlePath]; ok {
				errs = append(errs, BundleImageAlreadyAddedErr{ErrorString: fmt.Sprintf("Bundle %s already exists", image.Bundle.BundleImage)})
				continue
			}
		}
		channels, err := i.querier.ListChannels(context.TODO(), image.Bundle.Package)
		if err != nil {
			return err
		}

		for _, channel := range channels {
			bundle, err := i.querier.GetBundle(context.TODO(), image.Bundle.Package, channel, image.Bundle.Name)
			if err != nil {
				// Assume that if we can not find a bundle for the package, channel and or CSV Name that this is safe to add
				continue
			}
			if bundle != nil {
				if !i.overwrite {
					// raise error that this package + channel + csv combo is already in the db
					errs = append(errs, PackageVersionAlreadyAddedErr{ErrorString: "Bundle already added that provides package and csv"})
					break
				}
				// ensure overwrite is not in the middle of a channel (i.e. nothing replaces it)
				_, err = i.querier.GetBundleThatReplaces(context.TODO(), image.Bundle.Name, image.Bundle.Package, channel)
				if err != nil {
					if err.Error() == fmt.Errorf("no entry found for %s %s", image.Bundle.Package, channel).Error() {
						// overwrite is not replaced by any other bundle
						validOverwrite = true
						continue
					}
					errs = append(errs, err)
					break
				}
				// This bundle is in this channel but is not the head of this channel
				errs = append(errs, OverwriteErr{ErrorString: "Cannot overwrite a bundle that is not at the head of a channel using --overwrite-latest"})
				validOverwrite = false
				break
			}
		}
		if validOverwrite {
			if _, ok := attemptedOverwritesPerPackage[image.Bundle.Package]; ok {
				errs = append(errs, OverwriteErr{ErrorString: "Cannot overwrite more than one bundle at a time for a given package using --overwrite-latest"})
				break
			}
			attemptedOverwritesPerPackage[image.Bundle.Package] = struct{}{}
		}
	}

	return utilerrors.NewAggregate(errs)
}

func (i *DirectoryPopulator) loadManifests(imagesToAdd []*ImageInput, imagesToReAdd []*ImageInput, mode Mode) error {
	// global sanity checks before insertion
	if err := i.globalSanityCheck(imagesToAdd); err != nil {
		return err
	}

	switch mode {
	case ReplacesMode:
		// TODO: This is relatively inefficient. Ideally, we should be able to use a replaces
		// graph loader to construct what the graph would look like with a set of new bundles
		// and use that to return an error if it's not valid, rather than insert one at a time
		// and reinspect the database.
		//
		// Additionally, it would be preferrable if there was a single database transaction
		// that took the updated graph as a whole as input, rather than inserting bundles of the
		// same package linearly.
		for pkg := range i.overwriteDirMap {
			// TODO: If this succeeds but the add fails there will be a disconnect between
			// the registry and the index. Loading the bundles in a single transactions as
			// described above would allow us to do the removable in that same transaction
			// and ensure that rollback is possible.
			if err := i.loader.RemovePackage(pkg); err != nil {
				return err
			}
		}

		var errs []error
		stream, err := NewReplacesInputStream(i.graphLoader, append(imagesToAdd, imagesToReAdd...))
		if err != nil {
			errs = append(errs, fmt.Errorf("Input error: %s", err))
			// Don't return yet since stream may be partially initialized and still useful
		}
		if stream == nil {
			return utilerrors.NewAggregate(errs)
		}

		for !stream.Empty() {
			next, err := stream.Next()
			if err != nil {
				errs = append(errs, err)
				break
			}

			if err = i.loadManifestsReplaces(next.Bundle); err != nil {
				errs = append(errs, err)
				break
			}

		}

		if len(errs) > 0 {
			return utilerrors.NewAggregate(errs)
		}
	case SemVerMode:
		for _, image := range imagesToAdd {
			if err := i.loadManifestsSemver(image.Bundle, image.AnnotationsFile, false); err != nil {
				return err
			}
		}
	case SkipPatchMode:
		for _, image := range imagesToAdd {
			if err := i.loadManifestsSemver(image.Bundle, image.AnnotationsFile, true); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("Unsupported update mode")
	}

	// Finally let's delete all the old bundles
	if err := i.loader.ClearNonHeadBundles(); err != nil {
		return fmt.Errorf("Error deleting previous bundles: %s", err)
	}

	return nil
}

var packageContextKey = "package"

// ContextWithPackage adds a package value to a context.
func ContextWithPackage(ctx context.Context, pkg string) context.Context {
	return context.WithValue(ctx, packageContextKey, pkg)
}

// PackageFromContext returns the package value of the context if set, returns false if unset.
func PackageFromContext(ctx context.Context) (string, bool) {
	pkg, ok := ctx.Value(packageContextKey).(string)
	return pkg, ok
}

func (i *DirectoryPopulator) loadManifestsReplaces(bundle *Bundle) error {
	ctx := ContextWithPackage(context.TODO(), bundle.Package)
	bundles, err := i.querier.ListRegistryBundles(ctx)
	if err != nil {
		return fmt.Errorf("failed to list registry bundles: %s", err)
	}

	// Add the new bundle and get the semver informed package manifest
	bundles = append(bundles, bundle)
	packageManifest, err := SemverPackageManifest(bundles)
	if err != nil {
		return fmt.Errorf("failed to generate semver informed package manifest: %s", err)
	}

	if err := i.loadOperatorBundle(*packageManifest, bundle); err != nil {
		return fmt.Errorf("Error adding package %s", err)
	}

	return nil
}

func (i *DirectoryPopulator) loadManifestsSemver(bundle *Bundle, annotations *AnnotationsFile, skippatch bool) error {
	graph, err := i.graphLoader.Generate(bundle.Package)
	if err != nil && !errors.Is(err, ErrPackageNotInDatabase) {
		return err
	}

	// add to the graph
	bundleLoader := BundleGraphLoader{}
	updatedGraph, err := bundleLoader.AddBundleToGraph(bundle, graph, annotations, skippatch)
	if err != nil {
		return err
	}

	if err := i.loader.AddBundleSemver(updatedGraph, bundle); err != nil {
		return fmt.Errorf("error loading bundle into db: %s", err)
	}

	return nil
}

// loadBundle takes the directory that a CSV is in and assumes the rest of the objects in that directory
// are part of the bundle.
func loadBundle(csvName string, dir string) (*Bundle, error) {
	log := logrus.WithFields(logrus.Fields{"dir": dir, "load": "bundle"})
	files, err := ioutil.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	bundle := &Bundle{
		Name: csvName,
	}
	for _, f := range files {
		log = log.WithField("file", f.Name())
		if f.IsDir() {
			log.Info("skipping directory")
			continue
		}

		if strings.HasPrefix(f.Name(), ".") {
			log.Info("skipping hidden file")
			continue
		}

		log.Info("loading bundle file")
		var (
			obj  = &unstructured.Unstructured{}
			path = filepath.Join(dir, f.Name())
		)
		if err = DecodeFile(path, obj); err != nil {
			log.WithError(err).Debugf("could not decode file contents for %s", path)
			continue
		}

		// Don't include other CSVs in the bundle
		if obj.GetKind() == "ClusterServiceVersion" && obj.GetName() != csvName {
			continue
		}

		if obj.Object != nil {
			bundle.Add(obj)
		}
	}

	return bundle, nil
}

// findCSV looks through the bundle directory to find a csv
func (i *ImageInput) findCSV(manifests string) (*unstructured.Unstructured, error) {
	log := logrus.WithFields(logrus.Fields{"dir": i.from, "find": "csv"})

	files, err := ioutil.ReadDir(manifests)
	if err != nil {
		return nil, fmt.Errorf("unable to read directory %s: %s", manifests, err)
	}

	for _, f := range files {
		log = log.WithField("file", f.Name())
		if f.IsDir() {
			log.Info("skipping directory")
			continue
		}

		if strings.HasPrefix(f.Name(), ".") {
			log.Info("skipping hidden file")
			continue
		}

		var (
			obj  = &unstructured.Unstructured{}
			path = filepath.Join(manifests, f.Name())
		)
		if err = DecodeFile(path, obj); err != nil {
			log.WithError(err).Debugf("could not decode file contents for %s", path)
			continue
		}

		if obj.GetKind() != clusterServiceVersionKind {
			continue
		}

		return obj, nil
	}

	return nil, fmt.Errorf("no csv found in bundle")
}

// loadOperatorBundle adds the package information to the loader's store
func (i *DirectoryPopulator) loadOperatorBundle(manifest PackageManifest, bundle *Bundle) error {
	if manifest.PackageName == "" {
		return nil
	}

	if err := i.loader.AddBundlePackageChannels(manifest, bundle); err != nil {
		return fmt.Errorf("error loading bundle into db: %s", err)
	}

	return nil
}

// SemverPackageManifest generates a PackageManifest from a set of bundles, determining channel heads and the default channel using semver.
// Bundles with the highest version field (according to semver) are chosen as channel heads, and the default channel is taken from the last,
// highest versioned bundle in the entire set to define it.
// The given bundles must all belong to the same package or an error is thrown.
func SemverPackageManifest(bundles []*Bundle) (*PackageManifest, error) {
	type bundleVersion struct {
		name    string
		version semver.Version

		// Keep track of the number of times we visit each version so we can tell if a head is contested
		count int
	}
	heads := map[string]bundleVersion{}

	var (
		pkgName        string
		defaultChannel string
		maxVersion     bundleVersion
	)

	for _, bundle := range bundles {
		if pkgName != "" && pkgName != bundle.Package {
			return nil, fmt.Errorf("more than one package in input")
		}
		pkgName = bundle.Package

		rawVersion, err := bundle.Version()
		if err != nil {
			return nil, fmt.Errorf("error getting bundle %s version: %s", bundle.Name, err)
		}
		if rawVersion == "" {
			// If a version isn't provided by the bundle, give it a dummy zero version
			// The thought is that properly versioned bundles will always be non-zero
			rawVersion = "0.0.0-z"
		}

		version, err := semver.Parse(rawVersion)
		if err != nil {
			return nil, fmt.Errorf("error parsing bundle %s version %s: %s", bundle.Name, rawVersion, err)
		}
		current := bundleVersion{
			name:    bundle.Name,
			version: version,
			count:   1,
		}

		for _, channel := range bundle.Channels {
			head, ok := heads[channel]
			if !ok {
				heads[channel] = current
				continue
			}

			if version.LT(head.version) {
				continue
			}

			if version.EQ(head.version) {
				// We have a duplicate version, add the count
				current.count += head.count
			}

			// Current >= head
			heads[channel] = current
		}

		// Set max if bundle is greater
		if version.LT(maxVersion.version) {
			continue
		}

		if version.EQ(maxVersion.version) {
			current.count += maxVersion.count
		}

		// Current >= maxVersion
		maxVersion = current
		if annotations := bundle.Annotations; annotations != nil && annotations.DefaultChannelName != "" {
			// Take it when you can get it
			defaultChannel = annotations.DefaultChannelName
		}
	}

	if maxVersion.count > 1 {
		return nil, fmt.Errorf("more than one bundle with maximum version %s", maxVersion.version)
	}

	pkg := &PackageManifest{
		PackageName:        pkgName,
		DefaultChannelName: defaultChannel,
	}
	defaultFound := len(heads) == 1 && defaultChannel == ""
	for channel, head := range heads {
		if head.count > 1 {
			return nil, fmt.Errorf("more than one potential channel head for %s", channel)
		}
		if len(heads) == 1 {
			// Only one possible default channel
			pkg.DefaultChannelName = channel
		}
		defaultFound = defaultFound || channel == defaultChannel
		pkg.Channels = append(pkg.Channels, PackageChannel{
			Name:           channel,
			CurrentCSVName: head.name,
		})
	}

	if !defaultFound {
		return nil, fmt.Errorf("unable to determine default channel")
	}

	return pkg, nil
}

// DecodeFile decodes the file at a path into the given interface.
func DecodeFile(path string, into interface{}) error {
	if into == nil {
		panic("programmer error: decode destination must be instantiated before decode")
	}

	fileReader, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("unable to read file %s: %s", path, err)
	}
	defer fileReader.Close()

	decoder := yaml.NewYAMLOrJSONDecoder(fileReader, 30)

	return decoder.Decode(into)
}
