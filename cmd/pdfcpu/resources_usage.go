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

const (
	usageLongImportImages = `Turn image files into a PDF page sequence and write the result to outFile.
If outFile already exists the page sequence will be appended.

Each imageFile will be rendered to a separate page.
In its simplest form this converts an image into a PDF: "pdfcpu import img.pdf img.jpg"

description ... dimensions, formsize, position, offset, scale factor, boxes
    outFile ... output PDF file, use - to write to stdout
  imageFile ... a list of image files, use - to read one image from stdin

  <description> is a comma separated configuration string containing:
  
  optional entries:

      (defaults: "dim:595 842, f:A4, pos:full, off:0 0, sc:0.5 rel, dpi:72, gray:off, sepia:off")
  
   dimensions:      (width height) in given display unit eg. '400 200' setting the media box
  
   formsize:        eg. A4, Letter, Legal...
                    Append 'L' to enforce landscape mode. (eg. A3L)
                    Append 'P' to enforce portrait mode. (eg. TabloidP)
                    Please refer to "pdfcpu paper" for a comprehensive list of defined paper sizes.
                    "papersize" is also accepted.
  
   position:        one of 'full' or the anchors:
                        tl|top-left     tc|top-center      tr|top-right
                         l|left          c|center           r|right
                        bl|bottom-left  bc|bottom-center   br|bottom-right
  
   offset:          (dx dy) in given display unit eg. '15 20'
  
   scalefactor:     0.0 <= x <= 1.0 followed by optional 'abs|rel' or 'a|r'
  
   dpi:             apply desired dpi
  
   gray:            Convert to grayscale (on/off, true/false, t/f)
  
   sepia:           Apply sepia effect (on/off, true/false, t/f)
  
   backgroundcolor: "bgcolor" is also accepted.

  Only one of dimensions or formsize is allowed.
  position: full => image dimensions equal page dimensions.
  All configuration string parameters support completion.
  
  e.g. "f:A5, pos:c"                                ... render the image centered on A5 with relative scaling 0.5.'
       "dim:300 600, pos:bl, off:20 20, sc:1.0 abs" ... render the image anchored to bottom left corner with offset 20,20 and abs. scaling 1.0.
       "pos:full"                                   ... render the image to a page with corresponding dimensions.
       "f:A4, pos:c, dpi:300"                       ... render the image centered on A4 respecting a destination resolution of 300 dpi.

Pipeline examples:
  aws s3 cp s3://acme-assets/logo.png - \
     | pdfcpu import - - \
     | aws s3 cp - s3://acme-assets/logo.pdf

  cat photo.jpg \
     | pdfcpu import 'form:A4, pos:c' - - > photo.pdf`

	usageLongFonts = `Print a list of supported fonts (includes the 14 PDF core fonts).
Install given True Type fonts(.ttf) or True Type collections(.ttc) for usage in stamps/watermarks.
Create single page PDF cheat sheets in current dir.`

	usageLongImages = `Manage images.

     pages ... Please refer to "pdfcpu selectedpages"
    inFile ... input PDF file, use - to read from stdin
 imageFile ... image file
   outFile ... output PDF file, use - to write to stdout for update
     objNr ... obj# from "pdfcpu images list"
    pageNr ... Page from "pdfcpu images list"
        Id ... Id from "pdfcpu images list"

    Example: pdfcpu images list gallery.pdf
             gallery.pdf:
             1 images available (1.8 MB)
             Page Obj# │ Id  │ Type  SoftMask ImgMask │ Width │ Height │ ColorSpace Comp bpc Interp │   Size │ Filters
             ━━━━━━━━━━┿━━━━━┿━━━━━━━━━━━━━━━━━━━━━━━━┿━━━━━━━┿━━━━━━━━┿━━━━━━━━━━━━━━━━━━━━━━━━━━━━┿━━━━━━━━┿━━━━━━━━━━━━
                1    3 │ Im0 │ image                  │  1268 │    720 │  DeviceRGB    3   8    *   │ 1.8 MB │ FlateDecode

             # Extract all images into the current dir
             pdfcpu images extract gallery.pdf .
             extracting images from gallery.pdf into ./ ...
             optimizing...
             writing gallery_1_Im0.png

             # Update image with Id=Im0 on page=1 with gallery_1_Im0.png
             pdfcpu images update gallery.pdf gallery_1_Im0.png
             pdfcpu images update gallery.pdf gallery_1_Im0.png out.pdf

             # Update image object 3 with logo.png
             pdfcpu images update gallery.pdf logo.png 3
             pdfcpu images update gallery.pdf logo.png out.pdf 3

             # update image with Id=Im0 on page=1 with logo.jpg
             pdfcpu images update gallery.pdf logo.jpg 1 Im0
             pdfcpu images update gallery.pdf logo.jpg out.pdf 1 Im0

   Pipeline examples:
             aws s3 cp s3://acme-assets/gallery.pdf - \
                | pdfcpu images list -

             aws s3 cp s3://acme-assets/gallery.pdf - \
                | pdfcpu images extract - ./images

             aws s3 cp s3://acme-assets/gallery.pdf - \
                | pdfcpu images update - logo.jpg - 1 Im0 \
                | aws s3 cp - s3://acme-assets/gallery-updated.pdf
    `

	usageLongAttach = `Manage embedded file attachments.

    inFile ... input PDF file, use - to read from stdin
      file ... attachment
      desc ... description (optional)
    outDir ... output directory

    Adding attachments:
           pdfcpu attachment add test.pdf test.mp3 test.mkv

    Adding attachments with description:
           pdfcpu attachment add "test.mp3, audio" "test.mkv, video"

    Extract all attachments:
           pdfcpu attachments extract .

    Remove all attachments:
           pdfcpu attach remove test.pdf

Pipeline examples:
   aws s3 cp s3://acme-archive/report.pdf - \
      | pdfcpu attachments list -

   aws s3 cp s3://acme-archive/report.pdf - \
      | pdfcpu attachments add - source.xlsx > report-with-source.pdf

   cat report-with-source.pdf \
      | pdfcpu attachments remove - source.xlsx > report-without-source.pdf

   aws s3 cp s3://acme-archive/report.pdf - \
      | pdfcpu attachments extract - ./attachments
	`

	usageLongPortfolio = `Manage portfolio entries.

    inFile ... input PDF file, use - to read from stdin
      file ... attachment
      desc ... description (optional)
    outDir ... output directory

    Adding attachments to portfolio:
           pdfcpu portfolio add test.pdf test.mp3 test.mkv

    Adding attachments to portfolio with description:
           pdfcpu portfolio add test.pdf "test.mp3, Test sound file" "test.mkv, Test video file"

Pipeline examples:
   aws s3 cp s3://acme-dataroom/package.pdf - \
      | pdfcpu portfolio list -

   aws s3 cp s3://acme-dataroom/package.pdf - \
      | pdfcpu portfolio add - contract.pdf > package-with-contract.pdf

   cat package-with-contract.pdf \
      | pdfcpu portfolio remove - contract.pdf > package-without-contract.pdf

   aws s3 cp s3://acme-dataroom/package.pdf - \
      | pdfcpu portfolio extract - ./portfolio
    `

	usageLongKeywords = `Manage keywords.

    inFile ... input PDF file, use - to read from stdin
   outFile ... output PDF file, use - to write to stdout
   keyword ... search keyword

    Eg. adding two keywords:
           pdfcpu keywords add test.pdf music 'virtual instruments'

        remove all keywords:
           pdfcpu keywords remove test.pdf

Pipeline examples:
   aws s3 cp s3://acme-assets/brochure.pdf - \
      | pdfcpu keywords list -

   aws s3 cp s3://acme-assets/brochure.pdf - \
      | pdfcpu keywords add - - approved campaign-2026 \
      | aws s3 cp - s3://acme-assets/brochure-tagged.pdf

   aws s3 cp s3://acme-assets/brochure-tagged.pdf - \
      | pdfcpu keywords remove - - campaign-2026 \
      | aws s3 cp - s3://acme-assets/brochure-untagged.pdf
    `

	usageLongProperties = `Manage document properties.

       inFile ... input PDF file, use - to read from stdin
      outFile ... output PDF file, use - to write to stdout
nameValuePair ... 'name = value'
         name ... property name

     Eg. adding one property:   pdfcpu properties add test.pdf 'key = value'
         adding two properties: pdfcpu properties add test.pdf 'key1 = val1' 'key2 = val2'

         remove all properties: pdfcpu properties remove test.pdf

Pipeline examples:
   aws s3 cp s3://acme-assets/brochure.pdf - \
      | pdfcpu properties list -

   aws s3 cp s3://acme-assets/brochure.pdf - \
      | pdfcpu properties add - - 'Subject = Product Launch' \
      | aws s3 cp - s3://acme-assets/brochure-described.pdf

   aws s3 cp s3://acme-assets/brochure-described.pdf - \
      | pdfcpu properties remove - - Subject \
      | aws s3 cp - s3://acme-assets/brochure-clean-meta.pdf
     `
	usageBoxDescription = `
box:

   A rectangular region in user space describing one of:

      media box:  boundaries of the physical medium on which the page is to be printed.
       crop box:  region to which the contents of the page shall be clipped (cropped) when displayed or printed.
      bleed box:  region to which the contents of the page shall be clipped when output in a production environment.
       trim box:  intended dimensions of the finished page after trimming.
        art box:  extent of the page’s meaningful content as intended by the page’s creator.

   Please refer to the PDF Specification 14.11.2 Page Boundaries for details.

   All values are in given display unit (po, in, mm, cm)

   General rules:
      The media box is mandatory and serves as default for the crop box and is its parent box.
      The crop box serves as default for art box, bleed box and trim box and is their parent box.

   Arbitrary rectangular region in user space:
      [0 10 200 150]       lower left corner at (0/10), upper right corner at (200/150)
                           or xmin:0 ymin:10 xmax:200 ymax:150

   Expressed as margins within parent box:
      "0.5 0.5 20 20"      absolute, top:.5 right:.5 bottom:20 left:20
      "0.5 0.5 .1 .1 abs"  absolute, top:.5 right:.5 bottom:.1 left:.1
      "0.5 0.5 .1 .1 rel"  relative, top:.5 right:.5 bottom:20 left:20
      "10"                 absolute, top,right,bottom,left:10
      "10 5"               absolute, top,bottom:10  left,right:5
      "10 5 15"            absolute, top:10 left,right:5 bottom:15
      "5%"                 relative, top,right,bottom,left:5% of parent box width/height
      ".1 .5"              absolute, top,bottom:.1  left,right:.5
      ".1 .3 rel"          relative, top,bottom:.1=10%  left,right:.3=30%
      "-10"                absolute, top,right,bottom,left:-10 relative to parent box (for crop box the media box gets expanded)

   Anchored within parent box, use dim and optionally pos, off:
      "dim: 200 300 abs"                   centered, 200x300 display units
      "pos:c, off:0 0, dim: 200 300 abs"   centered, 200x300 display units
      "pos:tl, off:5 5, dim: 50% 50% rel"  anchored to top left corner, 50% width/height of parent box, offset by 5/5 display units
      "pos:br, off:-5 -5, dim: .5 .5 rel"  anchored to bottom right corner, 50% width/height of parent box, offset by -5/-5 display units


`
)
