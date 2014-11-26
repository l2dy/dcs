// vim:ts=4:sw=4:noexpandtab
// These handlers serve server-rendered pages for clients without JavaScript.
// The templates contain a bit of JavaScript that will automatically redirect
// to the more interactive version so that browsers that _do_ have JavaScript
// but follow a link will not end up in the server-rendered version.
package main

import (
	"encoding/json"
	"fmt"
	"github.com/Debian/dcs/cmd/dcs-web/common"
	"hash/fnv"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func maybeAppendContext(context []string, line string) []string {
	if strings.TrimSpace(line) != "" {
		replaced := line
		for strings.HasPrefix(replaced, "\t") {
			replaced = strings.Replace(replaced, "\t", "    ", 1)
		}
		return append(context, replaced)
	} else {
		return context
	}
}

func splitPath(path string) (sourcePackage string, relativePath string) {
	for i := 0; i < len(path); i++ {
		if path[i] == '_' {
			sourcePackage = path[:i]
			relativePath = path[i:]
			return
		}
	}
	return
}

// NB: Updates to this function must also be performed in static/instant.js.
func updatePagination(currentpage int, resultpages int, baseurl string) string {
	result := `<strong>Pages:</strong> `
	if currentpage > 0 {
		result = result + `<a href="` + baseurl + `&page=0">1</a> `
		result = result + `<span>&lt;</span> `
	}
	start := currentpage - 5
	if currentpage > 0 && 1 > start {
		start = 1
	}
	if currentpage == 0 && 0 > start {
		start = 0
	}
	end := resultpages
	if currentpage >= 5 && currentpage+5 < end {
		end = currentpage + 5
	}
	if currentpage < 5 && 10 < end {
		end = 10
	}

	for i := start; i < end; i++ {
		result = result + `<a style="`
		if i == currentpage {
			result = result + "font-weight: bold"
		}
		result = result + `" href="` + baseurl + `&page=` + strconv.Itoa(i) + `">` + strconv.Itoa(i+1) + `</a> `
	}

	if currentpage < (resultpages - 1) {
		result = result + `<span>&gt;</span> `
	}

	if end < resultpages {
		result = result + `… <a href="` + baseurl + `&page=` + strconv.Itoa(resultpages-1) + `">` + strconv.Itoa(resultpages) + `</a>`
	}

	return result
}

// q= search term
// page= page number
// TODO: perpkg= per-package grouping
func Search(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Could not parse form data", http.StatusInternalServerError)
		return
	}

	src := r.RemoteAddr
	if r.Form.Get("q") == "" {
		http.Error(w, "Empty query", http.StatusNotFound)
		return
	}

	// We encode a URL that contains _only_ the q parameter.
	q := url.Values{"q": []string{r.Form.Get("q")}}.Encode()

	pageStr := r.Form.Get("page")
	if pageStr == "" {
		pageStr = "0"
	}
	page, err := strconv.Atoi(pageStr)
	if err != nil {
		http.Error(w, "Invalid page parameter", http.StatusBadRequest)
		return
	}

	// Uniquely (well, good enough) identify this query for a couple of minutes
	// (as long as we want to cache results). We could try to normalize the
	// query before hashing it, but that seems hardly worth the complexity.
	h := fnv.New64()
	io.WriteString(h, q)
	queryid := fmt.Sprintf("%x", h.Sum64())

	log.Printf("getquery(%q, %q, %q)\n", queryid, src, q)

	maybeStartQuery(queryid, src, q)
	if !queryCompleted(queryid) {
		if err := common.Templates.ExecuteTemplate(w, "placeholder.html", map[string]interface{}{
			"q":       r.Form.Get("q"),
			"version": common.Version,
		}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		return
	}

	log.Printf("[%s] server-rendering page %d\n", queryid, page)

	dir := filepath.Join(*queryResultsPath, queryid)
	name := filepath.Join(dir, fmt.Sprintf("page_%d.json", page))
	resultsFile, err := os.Open(name)
	if err != nil {
		http.Error(w,
			fmt.Sprintf("Could not open results file on disk: %v", err),
			http.StatusInternalServerError)
		return
	}
	defer resultsFile.Close()

	var results []Result
	if err := json.NewDecoder(resultsFile).Decode(&results); err != nil {
		http.Error(w,
			fmt.Sprintf("Could not parse results from disk: %v", err),
			http.StatusInternalServerError)
		return
	}

	// XXX: Using a dcsregexp.Match anonymous struct member doesn’t work,
	// because we need to assign to the members to get the data from Result
	// over into HalfRenderedResult.
	type HalfRenderedResult struct {
		Path          string
		Line          int
		PathRank      float32
		Ranking       float32
		SourcePackage string
		RelativePath  string
		Context       template.HTML
	}

	halfrendered := make([]HalfRenderedResult, len(results))
	for idx, result := range results {
		var context []string
		context = maybeAppendContext(context, result.Ctxp2)
		context = maybeAppendContext(context, result.Ctxp1)
		context = append(context, "<strong>"+result.Context+"</strong>")
		context = maybeAppendContext(context, result.Ctxn1)
		context = maybeAppendContext(context, result.Ctxn2)

		sourcePackage, relativePath := splitPath(result.Path)

		halfrendered[idx] = HalfRenderedResult{
			Path:          result.Path,
			Line:          result.Line,
			PathRank:      result.PathRank,
			Ranking:       result.Ranking,
			SourcePackage: sourcePackage,
			RelativePath:  relativePath,
			Context:       template.HTML(strings.Join(context, "<br>")),
		}
	}

	packagesFile, err := os.Open(filepath.Join(dir, "packages.json"))
	if err != nil {
		http.Error(w,
			fmt.Sprintf("Could not open packages file on disk: %v", err),
			http.StatusInternalServerError)
		return
	}
	defer packagesFile.Close()

	var packages struct {
		Packages []string
	}

	if err := json.NewDecoder(packagesFile).Decode(&packages); err != nil {
		http.Error(w,
			fmt.Sprintf("Could not parse packages from disk: %v", err),
			http.StatusInternalServerError)
		return
	}

	basequery := r.URL.Query()
	basequery.Del("page")
	baseurl := r.URL
	baseurl.RawQuery = basequery.Encode()
	pagination := updatePagination(page, state[queryid].resultPages, baseurl.String())

	if err := common.Templates.ExecuteTemplate(w, "results.html", map[string]interface{}{
		"results":    halfrendered,
		"packages":   packages.Packages,
		"pagination": template.HTML(pagination),
		"q":          r.Form.Get("q"),
		"version":    common.Version,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}
