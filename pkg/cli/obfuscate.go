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

import "github.com/huzpsb/pdfcpu2/pkg/pdfcpu/model"

// ObfuscateCommand creates a new command to destroy the text extraction
// mappings of all embedded fonts of inFile by garbage filling the cmap
// tables of embedded font programs and all ToUnicode CMaps.
// Writes to outFile if supplied else overwrites inFile.
func ObfuscateCommand(inFile, outFile string, conf *model.Configuration) *Command {
	if conf == nil {
		conf = model.NewDefaultConfiguration()
		conf.Cmd = model.OBFUSCATE
	}
	return &Command{
		Mode:    model.OBFUSCATE,
		InFile:  &inFile,
		OutFile: &outFile,
		Conf:    conf}
}
