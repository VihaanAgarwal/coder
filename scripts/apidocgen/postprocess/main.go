package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"log"
	"os"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"

	"golang.org/x/xerrors"

	"github.com/coder/coder/v2/scripts/atomicwrite"
)

const (
	apiSubdir       = "reference/api"
	apiIndexFile    = "index.md"
	apiIndexContent = `---
title: API
---

Get started with the Coder API:

## Quickstart

Generate a token on your Coder deployment by visiting:

` + "````shell" + `
https://coder.example.com/settings/tokens
` + "````" + `

List your workspaces

` + "````shell" + `
# CLI
curl https://coder.example.com/api/v2/workspaces?q=owner:me \
-H "Coder-Session-Token: <your-token>"
` + "````" + `

## Use cases

See some common [use cases](../../reference/index.md#use-cases) for the REST API.

## Sections

<children>
  This page is rendered on https://coder.com/docs/reference/api. Refer to the other documents in the ` + "`api/`" + ` directory.
</children>
`
)

var (
	docsDirectory  string
	inMdFileSingle string

	sectionSeparator     = []byte("<!-- APIDOCGEN: BEGIN SECTION -->\n")
	nonAlphanumericRegex = regexp.MustCompile(`[^a-z0-9 ]+`)
	safeScalarRegex      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._/-]*$`)
)

// route is a docs manifest.json entry. Per-page metadata (title, description,
// icon_path, state) is mirrored into page front matter; the structural fields
// (path, children) stay in the manifest.
type route struct {
	Title       string   `json:"title,omitempty"`
	Description string   `json:"description,omitempty"`
	Path        string   `json:"path,omitempty"`
	IconPath    string   `json:"icon_path,omitempty"`
	State       []string `json:"state,omitempty"`
	Children    []route  `json:"children,omitempty"`
}

type manifest struct {
	Versions []string `json:"versions,omitempty"`
	Routes   []route  `json:"routes,omitempty"`
}

func main() {
	log.Println("Postprocess API docs")

	flag.StringVar(&docsDirectory, "docs-directory", "../../docs", "Path to Coder docs directory")
	flag.StringVar(&inMdFileSingle, "in-md-file-single", "", "Path to single Markdown file, output from widdershins.js")
	flag.Parse()

	if inMdFileSingle == "" {
		flag.Usage()
		log.Fatal("missing value for in-md-file-single")
	}

	sections, err := loadMarkdownSections()
	if err != nil {
		log.Fatal("can't load markdown sections: ", err)
	}

	err = prepareDocsDirectory()
	if err != nil {
		log.Fatal("can't prepare docs directory: ", err)
	}

	err = writeDocs(sections)
	if err != nil {
		log.Fatal("can't write docs directory: ", err)
	}

	log.Println("Done")
}

func loadMarkdownSections() ([][]byte, error) {
	log.Printf("Read the md-file-single: %s", inMdFileSingle)
	mdFile, err := os.ReadFile(inMdFileSingle)
	if err != nil {
		return nil, xerrors.Errorf("can't read the md-file-single: %w", err)
	}
	log.Printf("Read %dB", len(mdFile))

	sections := bytes.Split(mdFile, sectionSeparator)
	if len(sections) < 2 {
		return nil, xerrors.Errorf("At least 1 section is expected: %w", err)
	}
	sections = sections[1:] // Skip the first element which is the empty byte array
	log.Printf("Loaded %d sections", len(sections))
	return sections, nil
}

func prepareDocsDirectory() error {
	log.Println("Prepare docs directory")

	apiPath := path.Join(docsDirectory, apiSubdir)

	err := os.RemoveAll(apiPath)
	if err != nil {
		return xerrors.Errorf(`os.RemoveAll failed for "%s": %w`, apiPath, err)
	}

	err = os.MkdirAll(apiPath, 0o755)
	if err != nil {
		return xerrors.Errorf(`os.MkdirAll failed for "%s": %w`, apiPath, err)
	}
	return nil
}

func writeDocs(sections [][]byte) error {
	log.Println("Write docs to destination")

	manifestPath := path.Join(docsDirectory, "manifest.json")
	manifestFile, err := os.ReadFile(manifestPath)
	if err != nil {
		return xerrors.Errorf("can't read manifest file: %w", err)
	}
	log.Printf("Read manifest file: %dB", len(manifestFile))

	var m manifest
	err = json.Unmarshal(manifestFile, &m)
	if err != nil {
		return xerrors.Errorf("json.Unmarshal failed: %w", err)
	}

	// Index existing REST API routes by title so their curated metadata
	// (description, state, icon_path) flows into both the page front matter
	// and the regenerated manifest routes.
	existingByTitle := make(map[string]route)
	for _, r := range m.Routes {
		if r.Title != "Reference" {
			continue
		}
		for _, child := range r.Children {
			if child.Title != "REST API" {
				continue
			}
			for _, existing := range child.Children {
				existingByTitle[existing.Title] = existing
			}
		}
	}

	apiDir := path.Join(docsDirectory, apiSubdir)
	err = atomicwrite.File(path.Join(apiDir, apiIndexFile), []byte(apiIndexContent))
	if err != nil {
		return xerrors.Errorf(`can't write the index file: %w`, err)
	}

	type mdFile struct {
		title string
		path  string
	}
	var mdFiles []mdFile

	// Write .md files for grouped API method (Templates, Workspaces, etc.)
	for _, section := range sections {
		sectionName, err := extractSectionName(section)
		if err != nil {
			return xerrors.Errorf("can't extract section name: %w", err)
		}
		log.Printf("Write section: %s", sectionName)

		// Carry the manifest route's curated metadata into the front matter.
		r := existingByTitle[sectionName]
		r.Title = sectionName

		mdFilename := toMdFilename(sectionName)
		docPath := path.Join(apiDir, mdFilename)
		err = atomicwrite.File(docPath, frontMatterSection(section, r))
		if err != nil {
			return xerrors.Errorf(`can't write doc file "%s": %w`, docPath, err)
		}
		mdFiles = append(mdFiles, mdFile{
			title: sectionName,
			path:  "./" + path.Join(apiSubdir, mdFilename),
		})
	}

	// Sort API pages
	// The "General" section is expected to be always first.
	sort.Slice(mdFiles, func(i, j int) bool {
		if mdFiles[i].title == "General" {
			return true // "General" < ... - sorted
		}
		if mdFiles[j].title == "General" {
			return false // ... < "General" - not sorted
		}
		return slices.IsSorted([]string{mdFiles[i].title, mdFiles[j].title})
	})

	// Update manifest.json. Generated routes overwrite Title and Path;
	// existing state/description/icon_path are preserved (keyed by title) so
	// callouts like `state: ["experimental"]` survive regeneration.
	for i, r := range m.Routes {
		if r.Title != "Reference" {
			continue
		}
		for j, child := range r.Children {
			if child.Title != "REST API" {
				continue
			}

			var children []route
			for _, mdf := range mdFiles {
				docRoute := route{
					Title: mdf.title,
					Path:  mdf.path,
				}
				if existing, ok := existingByTitle[mdf.title]; ok {
					docRoute.State = existing.State
					docRoute.Description = existing.Description
					docRoute.IconPath = existing.IconPath
				}
				children = append(children, docRoute)
			}

			m.Routes[i].Children[j].Children = children
			break
		}
		break
	}

	manifestFile, err = json.MarshalIndent(m, "", "  ")
	if err != nil {
		return xerrors.Errorf("json.Marshal failed: %w", err)
	}

	err = atomicwrite.File(manifestPath, manifestFile)
	if err != nil {
		return xerrors.Errorf("can't write manifest file: %w", err)
	}
	log.Printf("Write manifest file: %dB", len(manifestFile))
	return nil
}

func extractSectionName(section []byte) (string, error) {
	scanner := bufio.NewScanner(bytes.NewReader(section))
	if !scanner.Scan() {
		return "", xerrors.Errorf("section header was expected")
	}

	header := scanner.Text()[2:] // Skip #<space>
	return strings.TrimSpace(header), nil
}

func toMdFilename(sectionName string) string {
	return nonAlphanumericRegex.ReplaceAllLiteralString(strings.ReplaceAll(strings.ToLower(sectionName), " ", ""), "-") + ".md"
}

// frontMatterSection replaces the leading "# {name}" heading of a raw API
// section with a YAML front matter block carrying the route's metadata: the
// title plus any description, icon_path, and state curated in the manifest.
// The docs site derives page metadata from front matter, so generated Markdown
// no longer needs a leading H1. Structural manifest fields (path, children)
// are intentionally not mirrored here.
func frontMatterSection(section []byte, r route) []byte {
	var body []byte
	if idx := bytes.IndexByte(section, '\n'); idx >= 0 {
		body = section[idx+1:]
	}
	body = bytes.TrimLeft(body, "\r\n")

	fm := "---\ntitle: " + yamlScalar(r.Title) + "\n"
	if r.Description != "" {
		fm += "description: " + yamlScalar(r.Description) + "\n"
	}
	if r.IconPath != "" {
		fm += "icon_path: " + yamlScalar(r.IconPath) + "\n"
	}
	if len(r.State) > 0 {
		fm += "state:\n"
		for _, s := range r.State {
			fm += "  - " + yamlScalar(s) + "\n"
		}
	}
	fm += "---\n\n"
	return append([]byte(fm), body...)
}

// yamlScalar renders s as a YAML scalar suitable for a front matter value.
// Simple values are emitted verbatim; anything else is JSON-encoded, which is
// valid YAML and safely quotes and escapes special characters.
func yamlScalar(s string) string {
	if safeScalarRegex.MatchString(s) {
		return s
	}
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}
