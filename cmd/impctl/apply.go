// Copyright Michael Robertson 2026
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mroberts91/imp/api/v1alpha1"
	"github.com/mroberts91/imp/internal/manifest"
	"github.com/mroberts91/imp/pkg/client"
)

func newApplyCmd(newClient func() *client.Client) *cobra.Command {
	var files []string
	cmd := &cobra.Command{
		Use:   "apply -f FILE",
		Short: "Apply manifests to the server",
		Long: `Apply manifests to the server.

Each document creates its object or replaces the object's spec wholesale:
removing a field from the manifest removes it from the live object.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			objects, err := loadManifests(files)
			if err != nil {
				return err
			}
			if len(objects) == 0 {
				return errors.New("no objects found in the given files")
			}
			c := newClient()
			ctx := cmd.Context()

			failed := 0
			for _, obj := range objects {
				outcome, err := applyObject(ctx, c, obj)
				if err != nil {
					failed++
					fmt.Fprintf(cmd.ErrOrStderr(), "%s/%s: ", strings.ToLower(obj.Kind), obj.Name)
					printApplyError(cmd.ErrOrStderr(), err)
					continue
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s/%s %s\n", strings.ToLower(obj.Kind), obj.Name, outcome)
			}
			if failed > 0 {
				return fmt.Errorf("%d of %d objects failed to apply", failed, len(objects))
			}
			return nil
		},
	}
	cmd.Flags().StringSliceVarP(&files, "filename", "f", nil, "manifest file or directory (repeatable)")
	cmd.MarkFlagRequired("filename") //nolint:errcheck // flag exists
	return cmd
}

// applyObject applies one manifest document and names the outcome the way
// kubectl does: created, configured, or unchanged. The outcome comes from
// comparing resourceVersions around the apply - the server's no-op
// short-circuit means an unchanged object keeps its resourceVersion.
func applyObject(ctx context.Context, c *client.Client, obj manifest.Object) (string, error) {
	priorRV := ""
	if prior, err := c.GetRaw(ctx, obj.Kind, obj.Name); err == nil {
		priorRV = rvOf(prior)
	} else if !errors.Is(err, v1alpha1.ErrNotFound) {
		return "", err
	}

	applied, err := c.Apply(ctx, obj.Kind, obj.Name, obj.Body)
	if err != nil {
		return "", err
	}
	switch appliedRV := rvOf(applied); {
	case priorRV == "":
		return "created", nil
	case appliedRV == priorRV:
		return "unchanged", nil
	default:
		return "configured", nil
	}
}

func printApplyError(w io.Writer, err error) {
	var inv *v1alpha1.InvalidError
	if errors.As(err, &inv) && len(inv.Errs) > 0 {
		fmt.Fprintln(w, "invalid:")
		for _, fe := range inv.Errs {
			fmt.Fprintf(w, "    * %s\n", fe.Error())
		}
		return
	}
	fmt.Fprintf(w, "%v\n", err)
}

// loadManifests expands files and directories (non-recursive, *.yaml and
// *.yml, sorted) into parsed objects.
func loadManifests(paths []string) ([]manifest.Object, error) {
	var objects []manifest.Object
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		files := []string{path}
		if info.IsDir() {
			files = nil
			for _, pattern := range []string{"*.yaml", "*.yml"} {
				matches, err := filepath.Glob(filepath.Join(path, pattern))
				if err != nil {
					return nil, err
				}
				files = append(files, matches...)
			}
			sort.Strings(files)
			if len(files) == 0 {
				return nil, fmt.Errorf("%s: no *.yaml or *.yml files", path)
			}
		}
		for _, file := range files {
			objs, err := manifest.ParseFile(file)
			if err != nil {
				return nil, err
			}
			objects = append(objects, objs...)
		}
	}
	return objects, nil
}

// rvOf extracts the resourceVersion from a raw object.
func rvOf(raw json.RawMessage) string {
	var peek struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &peek); err != nil {
		return ""
	}
	return peek.Metadata.ResourceVersion
}
