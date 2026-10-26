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

package cli

import (
	"github.com/huzpsb/pdfcpu2/pkg/pdfcpu/model"
	"github.com/huzpsb/pdfcpu2/pkg/render"
)

// RenderPagesCommand creates a new command to render pages into image files.
func RenderPagesCommand(inFile, outDir string, pageSelection []string, o *render.Options, conf *model.Configuration) *Command {
	if conf == nil {
		conf = model.NewDefaultConfiguration()
		conf.Cmd = model.RENDERPAGES
	}
	if o == nil {
		o = &render.Options{}
	}
	return &Command{
		Mode:          model.RENDERPAGES,
		InFile:        &inFile,
		OutDir:        &outDir,
		PageSelection: pageSelection,
		Render:        o,
		Conf:          conf}
}

// SolidifyCommand creates a new command to replace page content with its
// rendered image.
func SolidifyCommand(inFile, outFile string, pageSelection []string, o *render.Options, conf *model.Configuration) *Command {
	if conf == nil {
		conf = model.NewDefaultConfiguration()
		conf.Cmd = model.SOLIDIFYPAGES
	}
	if o == nil {
		o = &render.Options{}
	}
	return &Command{
		Mode:          model.SOLIDIFYPAGES,
		InFile:        &inFile,
		OutFile:       &outFile,
		PageSelection: pageSelection,
		Render:        o,
		Conf:          conf}
}
