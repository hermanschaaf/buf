package bufbuild

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/bufbuild/buf/internal/pkg/storage"
	"github.com/bufbuild/buf/internal/pkg/storage/storagepath"
	"github.com/bufbuild/buf/internal/pkg/util/utillog"
	"github.com/bufbuild/buf/internal/pkg/util/utilstring"
	"go.uber.org/zap"
)

type provider struct {
	logger *zap.Logger
}

func newProvider(logger *zap.Logger) *provider {
	return &provider{
		logger: logger,
	}
}

// GetProtoFileSetForBucket gets the set for the bucket and config.
func (p *provider) GetProtoFileSetForBucket(
	ctx context.Context,
	bucket storage.ReadBucket,
	roots []string,
	excludes []string,
) (ProtoFileSet, error) {
	defer utillog.Defer(p.logger, "get_proto_file_set_for_bucket")()

	config, err := newConfig(roots, excludes)
	if err != nil {
		return nil, err
	}
	// map from file path relative to root, to all actual file paths
	rootFilePathToRealFilePathMap := make(map[string]map[string]struct{})
	for _, root := range config.Roots {
		if walkErr := bucket.Walk(
			ctx,
			root,
			// all realFilePath values are already normalized and validated
			func(realFilePath string) error {
				if storagepath.Ext(realFilePath) != ".proto" {
					return nil
				}
				// get relative to root
				rootFilePath, err := storagepath.Rel(root, realFilePath)
				if err != nil {
					return err
				}
				// just in case
				rootFilePath, err = storagepath.NormalizeAndValidate(rootFilePath)
				if err != nil {
					return err
				}
				realFilePathMap, ok := rootFilePathToRealFilePathMap[rootFilePath]
				if !ok {
					realFilePathMap = make(map[string]struct{})
					rootFilePathToRealFilePathMap[rootFilePath] = realFilePathMap
				}
				realFilePathMap[realFilePath] = struct{}{}
				return nil
			},
		); walkErr != nil {
			return nil, walkErr
		}
	}

	rootFilePathToRealFilePath := make(map[string]string, len(rootFilePathToRealFilePathMap))
	for rootFilePath, realFilePathMap := range rootFilePathToRealFilePathMap {
		realFilePaths := make([]string, 0, len(realFilePathMap))
		for realFilePath := range realFilePathMap {
			realFilePaths = append(realFilePaths, realFilePath)
		}
		switch len(realFilePaths) {
		case 0:
			// we expect to always have at least one value, this is a system error
			return nil, fmt.Errorf("no real file path for file path %q", rootFilePath)
		case 1:
			rootFilePathToRealFilePath[rootFilePath] = realFilePaths[0]
		default:
			sort.Strings(realFilePaths)
			return nil, fmt.Errorf("file with path %s is within multiple roots at %v", rootFilePath, realFilePaths)
		}
	}

	if len(config.Excludes) == 0 {
		if len(rootFilePathToRealFilePath) == 0 {
			return nil, errors.New("no input files found that match roots")
		}
		return newProtoFileSet(config.Roots, rootFilePathToRealFilePath)
	}

	filteredRootFilePathToRealFilePath := make(map[string]string, len(rootFilePathToRealFilePath))
	excludeMap := utilstring.SliceToMap(config.Excludes)
	for rootFilePath, realFilePath := range rootFilePathToRealFilePath {
		if !storagepath.MapContainsMatch(excludeMap, storagepath.Dir(realFilePath)) {
			filteredRootFilePathToRealFilePath[rootFilePath] = realFilePath
		}
	}
	if len(filteredRootFilePathToRealFilePath) == 0 {
		return nil, errors.New("no input files found that match roots and excludes")
	}
	return newProtoFileSet(config.Roots, filteredRootFilePathToRealFilePath)
}

// GetSetForRealFilePaths gets the set for the real file paths and config.
//
// File paths will be validated to make sure they are within a root,
// unique relative to roots, and that they exist. If allowNotExist
// is true, files that do not exist will be filtered. This is useful
// for i.e. --limit-to-input-files.
func (p *provider) GetProtoFileSetForRealFilePaths(
	ctx context.Context,
	bucket storage.ReadBucket,
	roots []string,
	realFilePaths []string,
	realFilePathsAllowNotExist bool,
) (ProtoFileSet, error) {
	defer utillog.Defer(p.logger, "get_proto_file_set_for_real_file_paths")()

	config, err := newConfig(roots, nil)
	if err != nil {
		return nil, err
	}
	normalizedRealFilePaths := make(map[string]struct{}, len(realFilePaths))
	for _, realFilePath := range realFilePaths {
		normalizedRealFilePath, err := storagepath.NormalizeAndValidate(realFilePath)
		if err != nil {
			return nil, err
		}
		if _, ok := normalizedRealFilePaths[normalizedRealFilePath]; ok {
			return nil, fmt.Errorf("duplicate normalized file path %s", normalizedRealFilePath)
		}
		// check that the file exists primarily
		if _, err := bucket.Stat(ctx, normalizedRealFilePath); err != nil {
			if !storage.IsNotExist(err) {
				return nil, err
			}
			if !realFilePathsAllowNotExist {
				return nil, err
			}
		} else {
			normalizedRealFilePaths[normalizedRealFilePath] = struct{}{}
		}
	}

	rootMap := utilstring.SliceToMap(config.Roots)
	rootFilePathToRealFilePath := make(map[string]string, len(normalizedRealFilePaths))
	for normalizedRealFilePath := range normalizedRealFilePaths {
		matchingRootMap := storagepath.MapMatches(rootMap, normalizedRealFilePath)
		matchingRoots := make([]string, 0, len(matchingRootMap))
		for matchingRoot := range matchingRootMap {
			matchingRoots = append(matchingRoots, matchingRoot)
		}
		switch len(matchingRoots) {
		case 0:
			return nil, fmt.Errorf("file %s is not within any root %v", normalizedRealFilePath, config.Roots)
		case 1:
			normalizedRootFilePath, err := storagepath.Rel(matchingRoots[0], normalizedRealFilePath)
			if err != nil {
				return nil, err
			}
			// just in case
			// return system error as this would be an issue
			normalizedRootFilePath, err = storagepath.NormalizeAndValidate(normalizedRootFilePath)
			if err != nil {
				// This is a system error
				return nil, err
			}
			if otherRealFilePath, ok := rootFilePathToRealFilePath[normalizedRootFilePath]; ok {
				return nil, fmt.Errorf("file with path %s is within another root as %s at %s", normalizedRealFilePath, normalizedRootFilePath, otherRealFilePath)
			}
			rootFilePathToRealFilePath[normalizedRootFilePath] = normalizedRealFilePath
		default:
			sort.Strings(matchingRoots)
			// this should probably never happen due to how we are doing this with matching roots but just in case
			return nil, fmt.Errorf("file with path %s is within multiple roots at %v", normalizedRealFilePath, matchingRoots)
		}
	}

	if len(rootFilePathToRealFilePath) == 0 {
		return nil, errors.New("no input files specified")
	}
	return newProtoFileSet(config.Roots, rootFilePathToRealFilePath)
}
