// Package catalogcmd wires the `cr catalog` command surface.
package catalogcmd

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/modelcatalog"
	"github.com/open-cli-collective/codereview-cli/internal/view"
)

// Register attaches the catalog command tree to rootCmd.
func Register(rootCmd *cobra.Command, opts *root.Options) {
	cmd := &cobra.Command{
		Use:   "catalog",
		Short: "Inspect or refresh the model catalog",
	}
	cmd.AddCommand(newShowCommand(opts), newUpdateCommand(opts))
	rootCmd.AddCommand(cmd)
}

type catalogView struct {
	Digest   string                 `json:"digest"`
	Source   modelcatalog.Source    `json:"source"`
	Manifest modelcatalog.Manifest  `json:"manifest"`
	Runtimes []runtimeView          `json:"runtimes"`
	Models   []modelView            `json:"models"`
	Defaults []modelcatalog.Default `json:"defaults"`
	Pricing  []modelcatalog.Price   `json:"pricing"`
}

type runtimeView struct {
	ID            string `json:"id"`
	Provider      string `json:"provider"`
	Auth          string `json:"auth"`
	Harness       string `json:"harness"`
	Adapter       string `json:"adapter"`
	MaximumEffort string `json:"maximum_effort"`
}

type modelView struct {
	RuntimeID        string   `json:"runtime_id"`
	ModelID          string   `json:"model_id"`
	SupportedEfforts []string `json:"supported_efforts,omitempty"`
	Fast             bool     `json:"fast"`
	FastKnown        bool     `json:"fast_known"`
	Ultrafast        bool     `json:"ultrafast"`
	UltrafastKnown   bool     `json:"ultrafast_known"`
}

func newShowCommand(opts *root.Options) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Show the active catalog source, revision, and capabilities",
		Args:  exitcode.NoArgs("catalog show accepts no arguments"),
		RunE: func(_ *cobra.Command, _ []string) error {
			catalog, err := opts.CatalogSnapshot()
			if err != nil {
				return err
			}
			return render(opts.Stdout, jsonOutput, catalog)
		},
	}
	root.AddJSONFlag(cmd, &jsonOutput)
	return cmd
}

func newUpdateCommand(opts *root.Options) *cobra.Command {
	var url string
	var installDir string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "update [source-dir]",
		Short: "Validate and atomically install a catalog snapshot",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 1 {
				return exitcode.Usage(fmt.Errorf("catalog update accepts at most one source directory"))
			}
			if strings.TrimSpace(url) != "" && len(args) == 1 {
				return exitcode.Usage(fmt.Errorf("catalog update accepts either a source directory or --url"))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			update := modelcatalog.UpdateOptions{URL: url, InstallDir: installDir}
			if len(args) == 1 {
				update.SourceDir = args[0]
			}
			catalog, err := modelcatalog.Update(cmd.Context(), update)
			if err != nil {
				return err
			}
			return renderUpdate(opts.Stdout, jsonOutput, catalog)
		},
	}
	root.AddJSONFlag(cmd, &jsonOutput)
	cmd.Flags().StringVar(&url, "url", "", "Catalog base URL")
	cmd.Flags().StringVar(&installDir, "install-dir", "", "Catalog installation directory")
	return cmd
}

func render(w io.Writer, jsonOutput bool, catalog *modelcatalog.Catalog) error {
	value := buildView(catalog)
	if jsonOutput {
		return view.RenderJSON(w, value)
	}
	_, err := fmt.Fprintf(w, "Source: %s\nRevision: %s\nRuntimes: %d\nModels: %d\nDefaults: %d\nPricing rows: %d\n", value.Source.Kind, value.Manifest.Revision, len(value.Runtimes), len(value.Models), len(value.Defaults), len(value.Pricing))
	if err != nil {
		return err
	}
	for _, runtime := range value.Runtimes {
		if _, err := fmt.Fprintf(w, "Runtime %s (%s/%s/%s) max effort=%s\n", runtime.ID, runtime.Provider, runtime.Auth, runtime.Adapter, runtime.MaximumEffort); err != nil {
			return err
		}
	}
	for _, model := range value.Models {
		fast := "unknown"
		if model.FastKnown {
			fast = fmt.Sprintf("%t", model.Fast)
		}
		ultrafast := "unknown"
		if model.UltrafastKnown {
			ultrafast = fmt.Sprintf("%t", model.Ultrafast)
		}
		if _, err := fmt.Fprintf(w, "  %s/%s efforts=%s fast=%s ultrafast=%s\n", model.RuntimeID, model.ModelID, strings.Join(model.SupportedEfforts, "|"), fast, ultrafast); err != nil {
			return err
		}
	}
	return nil
}

func renderUpdate(w io.Writer, jsonOutput bool, catalog *modelcatalog.Catalog) error {
	if jsonOutput {
		return view.RenderJSON(w, struct {
			Source   modelcatalog.Source `json:"source"`
			Revision string              `json:"revision"`
		}{catalog.Source(), catalog.Revision()})
	}
	_, err := fmt.Fprintf(w, "Installed catalog revision %s (%s)\n", catalog.Revision(), catalog.Source().Kind)
	return err
}

func buildView(catalog *modelcatalog.Catalog) catalogView {
	runtimes := catalog.Runtimes()
	runtimeViews := make([]runtimeView, 0, len(runtimes))
	for _, runtime := range runtimes {
		runtimeViews = append(runtimeViews, runtimeView{ID: runtime.ID, Provider: runtime.Provider, Auth: runtime.Auth, Harness: runtime.Harness, Adapter: runtime.Adapter, MaximumEffort: runtime.MaximumEffort})
	}
	models := catalog.Models()
	modelViews := make([]modelView, 0, len(models))
	for _, model := range models {
		modelViews = append(modelViews, modelView{RuntimeID: model.RuntimeID, ModelID: model.ModelID, SupportedEfforts: model.SupportedEfforts, Fast: model.Fast, FastKnown: model.FastKnown, Ultrafast: model.Ultrafast, UltrafastKnown: model.UltrafastKnown})
	}
	sort.Slice(runtimeViews, func(i, j int) bool { return runtimeViews[i].ID < runtimeViews[j].ID })
	sort.Slice(modelViews, func(i, j int) bool {
		if modelViews[i].RuntimeID == modelViews[j].RuntimeID {
			return modelViews[i].ModelID < modelViews[j].ModelID
		}
		return modelViews[i].RuntimeID < modelViews[j].RuntimeID
	})
	return catalogView{Digest: catalog.Digest(), Source: catalog.Source(), Manifest: catalog.Manifest(), Runtimes: runtimeViews, Models: modelViews, Defaults: catalog.Defaults(), Pricing: catalog.Pricing()}
}
