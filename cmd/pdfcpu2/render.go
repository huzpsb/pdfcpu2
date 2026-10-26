/*
Copyright 2026 The pdfcpu Authors.

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

package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/huzpsb/pdfcpu2/internal/contextutil"
	"github.com/huzpsb/pdfcpu2/pkg/cli"
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/model"
	"github.com/huzpsb/pdfcpu2/pkg/render"
	"github.com/spf13/cobra"
)

// renderCmdOptions carries the rasterization flags shared by the render and
// solidify commands.
type renderCmdOptions struct {
	dpi     float64
	mode    string
	quality int
}

func defaultRenderCmdOptions() *renderCmdOptions {
	return &renderCmdOptions{dpi: 150, mode: "png", quality: 85}
}

func addRenderFlags(cmd *cobra.Command, opts *renderCmdOptions) {
	cmd.Flags().Float64Var(&opts.dpi, "dpi", opts.dpi, "resolution in dots per inch")
	cmd.Flags().StringVarP(&opts.mode, "mode", "m", opts.mode, "embedded image format: png | jpg")
	cmd.Flags().IntVar(&opts.quality, "quality", opts.quality, "JPEG quality (1..100)")
}

func (opts *renderCmdOptions) renderOptions() (*render.Options, error) {
	if opts.dpi <= 0 || opts.dpi > 4800 {
		return nil, errors.New("dpi must be in (0, 4800]")
	}
	format, err := render.NormalizeFormat(opts.mode)
	if err != nil {
		return nil, err
	}
	if opts.quality < 1 || opts.quality > 100 {
		return nil, errors.New("quality must be in 1..100")
	}
	return &render.Options{
		DPI:         opts.dpi,
		Format:      format,
		JPEGQuality: opts.quality,
	}, nil
}

func renderCmd() *cobra.Command {
	opts := defaultRenderCmdOptions()

	cmd := &cobra.Command{
		Use:   "render inFile outDir",
		Short: "Render selected pages into image files",
		Long:  usageLongRender,
		Args:  cobra.ExactArgs(2),
		RunE: wrapContextHandler(func(c context.Context, conf *model.Configuration, args []string) error {
			return handleRenderCommand(c, conf, args, opts)
		}),
	}
	addRenderFlags(cmd, opts)
	addSelectedPagesFlag(cmd)
	addPasswordFlags(cmd)

	return cmd
}

func solidifyCmd() *cobra.Command {
	opts := defaultRenderCmdOptions()

	cmd := &cobra.Command{
		Use:   "solidify inFile [outFile]",
		Short: "Replace page content with its rendered image",
		Long:  usageLongSolidify,
		Args:  cobra.RangeArgs(1, 2),
		RunE: wrapContextHandler(func(c context.Context, conf *model.Configuration, args []string) error {
			return handleSolidifyCommand(c, conf, args, opts)
		}),
	}
	addRenderFlags(cmd, opts)
	addSelectedPagesFlag(cmd)
	addPasswordFlags(cmd)

	return cmd
}

func handleRenderCommand(c context.Context, conf *model.Configuration, args []string, opts *renderCmdOptions) error {
	if err := contextutil.Check(c); err != nil {
		return err
	}
	o, err := opts.renderOptions()
	if err != nil {
		return fmt.Errorf("render: %w", err)
	}

	inFile := args[0]
	if err := inputPDFArg(conf, inFile); err != nil {
		return fmt.Errorf("render: %w", err)
	}
	outDir := args[1]
	if err := ensureOutputDirEmpty(outDir); err != nil {
		return fmt.Errorf("render: %w", err)
	}

	pages, err := parseSelectedPages()
	if err != nil {
		return fmt.Errorf("render: %w", err)
	}

	return runCommand(c, cli.RenderPagesCommand(inFile, outDir, pages, o, conf))
}

func handleSolidifyCommand(c context.Context, conf *model.Configuration, args []string, opts *renderCmdOptions) error {
	if err := contextutil.Check(c); err != nil {
		return err
	}
	o, err := opts.renderOptions()
	if err != nil {
		return fmt.Errorf("solidify: %w", err)
	}

	inFile, outFile, err := optionalOutputPDFArgs(conf, args)
	if err != nil {
		return fmt.Errorf("solidify: %w", err)
	}

	pages, err := parseSelectedPages()
	if err != nil {
		return fmt.Errorf("solidify: %w", err)
	}

	return runCommand(c, cli.SolidifyCommand(inFile, outFile, pages, o, conf))
}
