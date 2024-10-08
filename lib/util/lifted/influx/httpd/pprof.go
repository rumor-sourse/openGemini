package httpd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	httppprof "net/http/pprof"
	"runtime/pprof"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/influxdata/influxdb/models"
)

// handleProfiles determines which profile to return to the requester.
func (h *Handler) handleProfiles(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/debug/pprof/cmdline":
		httppprof.Cmdline(w, r)
	case "/debug/pprof/profile":
		httppprof.Profile(w, r)
	case "/debug/pprof/symbol":
		httppprof.Symbol(w, r)
	case "/debug/pprof/all":
		h.archiveProfilesAndQueries(w, r)
	case "/debug/pprof":
		// redirect /debug/pprof to /debug/pprof/ to maintain routing compatibility with the stdlib pprof.Index
		http.Redirect(w, r, "/debug/pprof/", http.StatusFound)
	default:
		httppprof.Index(w, r)
	}
}

// prof describes a profile name and a debug value, or in the case of a CPU
// profile, the number of seconds to collect the profile for.
type prof struct {
	Name  string
	Debug int64
}

// archiveProfilesAndQueries collects the following profiles:
//   - goroutine profile
//   - heap profile
//   - blocking profile
//   - mutex profile
//   - (optionally) CPU profile
//
// It also collects the following query results:
//
//   - SHOW SHARDS
//   - SHOW STATS
//   - SHOW DIAGNOSTICS
//
// All information is added to a tar archive and then compressed, before being
// returned to the requester as an archive file. Where profiles support debug
// parameters, the profile is collected with debug=1. To optionally include a
// CPU profile, the requester should provide a `cpu` query parameter, and can
// also provide a `seconds` parameter to specify a non-default profile
// collection time. The default CPU profile collection time is 30 seconds.
//
// Example request including CPU profile:
//
//	http://localhost:8086/debug/pprof/all?cpu=true&seconds=45
//
// The value after the `cpu` query parameter is not actually important, as long
// as there is something there.
func (h *Handler) archiveProfilesAndQueries(w http.ResponseWriter, r *http.Request) {
	var allProfs = []*prof{
		{Name: "goroutine", Debug: 1},
		{Name: "block", Debug: 1},
		{Name: "mutex", Debug: 1},
		{Name: "heap", Debug: 1},
	}

	// Capture a CPU profile?
	if r.FormValue("cpu") != "" {
		profile := &prof{Name: "cpu"}

		// For a CPU profile we'll use the Debug field to indicate the number of
		// seconds to capture the profile for.
		profile.Debug, _ = strconv.ParseInt(r.FormValue("seconds"), 10, 64)
		if profile.Debug <= 0 {
			profile.Debug = 30
		}
		allProfs = append([]*prof{profile}, allProfs...) // CPU profile first.
	}

	var (
		resp bytes.Buffer // Temporary buffer for entire archive.
		buf  bytes.Buffer // Temporary buffer for each profile/query result.
	)

	gz := gzip.NewWriter(&resp)
	tw := tar.NewWriter(gz)

	// Collect and write out profiles.
	for _, profile := range allProfs {
		if profile.Name == "cpu" {
			if err := pprof.StartCPUProfile(&buf); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}

			sleep(w, time.Duration(profile.Debug)*time.Second)
			pprof.StopCPUProfile()
		} else {
			prof := pprof.Lookup(profile.Name)
			if prof == nil {
				http.Error(w, "unable to find profile "+profile.Name, http.StatusInternalServerError)
				return
			}

			if err := prof.WriteTo(&buf, int(profile.Debug)); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}

		// Write the profile file's header.
		err := tw.WriteHeader(&tar.Header{
			Name: profile.Name + ".txt",
			Mode: 0600,
			Size: int64(buf.Len()),
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}

		// Write the profile file's data.
		if _, err := tw.Write(buf.Bytes()); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}

		// Reset the buffer for the next profile.
		buf.Reset()
	}

	// Collect and write out the queries.
	var allQueries = []struct {
		name string
		fn   func() ([]*models.Row, error)
	}{
		{"shards", h.showShards},
	}

	tabW := tabwriter.NewWriter(&buf, 8, 8, 1, '\t', 0)
	for _, query := range allQueries {
		rows, err := query.fn()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}

		for i, row := range rows {
			var out []byte
			// Write the columns
			for _, col := range row.Columns {
				out = append(out, []byte(col+"\t")...)
			}
			out = append(out, '\n')
			if _, err := tabW.Write(out); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}

			// Write all the values
			for _, val := range row.Values {
				out = out[:0]
				for _, v := range val {
					out = append(out, []byte(fmt.Sprintf("%v\t", v))...)
				}
				out = append(out, '\n')
				if _, err := tabW.Write(out); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
				}
			}

			// Write a final newline
			if i < len(rows)-1 {
				if _, err := tabW.Write([]byte("\n")); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
				}
			}
		}

		if err := tabW.Flush(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}

		err = tw.WriteHeader(&tar.Header{
			Name: query.name + ".txt",
			Mode: 0600,
			Size: int64(buf.Len()),
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}

		// Write the query file's data.
		if _, err := tw.Write(buf.Bytes()); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}

		// Reset the buffer for the next query.
		buf.Reset()
	}

	// Close the tar writer.
	if err := tw.Close(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}

	// Close the gzip writer.
	if err := gz.Close(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}

	// Return the gzipped archive.
	w.Header().Set("Content-Disposition", "attachment; filename=profiles.tar.gz")
	w.Header().Set("Content-Type", "application/gzip")
	io.Copy(w, &resp) // Nothing we can really do about an error at this point.
}

// showShards generates the same values that a StatementExecutor would if a
// SHOW SHARDS query was executed.
func (h *Handler) showShards() ([]*models.Row, error) {
	return h.MetaClient.ShowShards("", "", ""), nil
}

// Taken from net/http/pprof/pprof.go
func sleep(w http.ResponseWriter, d time.Duration) {
	var clientGone <-chan bool
	if cn, ok := w.(http.CloseNotifier); ok {
		clientGone = cn.CloseNotify()
	}
	select {
	case <-time.After(d):
	case <-clientGone:
	}
}
