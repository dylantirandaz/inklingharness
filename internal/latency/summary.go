package latency

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

type samples struct {
	durations []int64
	calls     int64
	failures  int64
	canceled  int64
}

func required(raw []byte, names ...string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for _, name := range names {
		if value, ok := fields[name]; !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return errors.New("missing or null required field")
		}
	}
	return nil
}

func validate(raw []byte, r *record) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(r); err != nil {
		return errors.New("invalid record schema")
	}
	var trailing json.RawMessage
	if decoder.Decode(&trailing) != io.EOF {
		return errors.New("trailing data")
	}
	if err := required(raw, "version", "model", "effort", "state", "ns", "dropped", "entries"); err != nil {
		return err
	}
	if r.Version != 1 {
		return errors.New("unsupported record version")
	}
	if r.Model == "" || r.Effort == "" || identifier(r.Model) != r.Model || identifier(r.Effort) != r.Effort || r.NS < 0 || r.Entries == nil || len(r.Entries) > maxEntries {
		return errors.New("invalid record metadata")
	}
	switch r.State {
	case "success", "error", "canceled":
	default:
		return errors.New("invalid run state")
	}
	var fields struct {
		Entries []json.RawMessage `json:"entries"`
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return errors.New("invalid entries")
	}
	for index, e := range r.Entries {
		if err := required(fields.Entries[index], "stage", "operation", "count", "failures", "canceled", "ns"); err != nil {
			return err
		}
		if !validStage(e.Stage) || operation(e.Operation) != e.Operation || e.Count < 1 || e.Failures < 0 || e.Canceled < 0 || e.Failures > e.Count || e.Canceled > e.Count-e.Failures || e.NS < 0 || e.Attempt < 0 || e.Attempt > 5 {
			return errors.New("invalid measurement")
		}
		switch e.Protocol {
		case "", "HTTP/1.0", "HTTP/1.1", "HTTP/2.0", "HTTP/3.0":
		default:
			return errors.New("unsupported HTTP protocol")
		}
		if e.Status != 0 && (e.Status < 100 || e.Status > 599) {
			return errors.New("invalid HTTP status")
		}
		if e.Stage != HTTP && (e.Protocol != "" || e.Status != 0 || e.Reused != nil) || e.Stage != Request && e.Usage != nil {
			return errors.New("metadata on wrong stage")
		}
		if e.Usage != nil {
			for _, value := range []*int{e.Usage.Input, e.Usage.Output, e.Usage.CacheRead, e.Usage.CacheCreation} {
				if value != nil && *value < 0 {
					return errors.New("invalid token count")
				}
			}
		}
	}
	return nil
}

// percentile requires nonempty values in ascending order.
func percentile(values []int64, fraction float64) time.Duration {
	index := int(math.Ceil(float64(len(values))*fraction)) - 1
	return time.Duration(values[max(0, index)])
}

// Summarize validates JSONL before writing a report. Failed/canceled runs and
// stages never contribute durations to successful-run percentile samples.
func Summarize(reader io.Reader, writer io.Writer) error {
	groups := make(map[string]*samples)
	add := func(key string, ns, calls, failures, canceled int64) {
		group := groups[key]
		if group == nil {
			group = &samples{}
			groups[key] = group
		}
		group.durations = append(group.durations, ns)
		group.calls += calls
		group.failures += failures
		group.canceled += canceled
	}
	var runs, failed, canceled, dropped, reuseYes, reuseNo, reuseAbsent, requests int64
	var cacheReported, cacheHits, cacheMisses, cacheTokens, cacheCreationReported, cacheCreationTokens, inputReported, inputTokens, outputReported, outputTokens int64
	protocols := make(map[string]int64)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	for line := 1; scanner.Scan(); line++ {
		var r record
		if err := validate(scanner.Bytes(), &r); err != nil {
			return fmt.Errorf("timings: line %d: %w", line, err)
		}
		runs++
		var fail, cancel int64
		if r.State == "error" {
			failed++
			fail = 1
		}
		if r.State == "canceled" {
			canceled++
			cancel = 1
		}
		dropped += int64(r.Dropped)
		prefix := r.Model + "\t" + r.Effort + "\t" + r.State + "\t"
		add(prefix+"run\tall\t"+r.State, r.NS, 1, fail, cancel)
		for _, e := range r.Entries {
			status := "success"
			if e.Failures > 0 {
				status = "error"
			}
			if e.Canceled > 0 {
				status = "canceled"
			}
			add(prefix+string(e.Stage)+"\t"+e.Operation+"\t"+status, e.NS, e.Count, e.Failures, e.Canceled)
			if e.Stage == HTTP {
				if e.Reused == nil {
					reuseAbsent++
				} else if *e.Reused {
					reuseYes++
				} else {
					reuseNo++
				}
				protocol := e.Protocol
				if protocol == "" {
					protocol = "unreported"
				}
				protocols[protocol]++
			}
			if e.Stage == Request {
				requests++
				if e.Usage != nil {
					if e.Usage.CacheRead != nil {
						cacheReported++
						cacheTokens += int64(*e.Usage.CacheRead)
						if *e.Usage.CacheRead > 0 {
							cacheHits++
						} else {
							cacheMisses++
						}
					}
					if e.Usage.CacheCreation != nil {
						cacheCreationReported++
						cacheCreationTokens += int64(*e.Usage.CacheCreation)
					}
					if e.Usage.Input != nil {
						inputReported++
						inputTokens += int64(*e.Usage.Input)
					}
					if e.Usage.Output != nil {
						outputReported++
						outputTokens += int64(*e.Usage.Output)
					}
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("timings: read records: %w", err)
	}
	if runs == 0 {
		return errors.New("timings: no records")
	}
	var report strings.Builder
	fmt.Fprintf(&report, "Runs: %d; failures: %d; canceled: %d; dropped measurements: %d\n", runs, failed, canceled, dropped)
	fmt.Fprintf(&report, "HTTP connection reuse: yes=%d no=%d unreported=%d\n", reuseYes, reuseNo, reuseAbsent)
	var protocolNames []string
	for name := range protocols {
		protocolNames = append(protocolNames, name)
	}
	sort.Strings(protocolNames)
	for _, name := range protocolNames {
		fmt.Fprintf(&report, "Protocol %s: %d attempts\n", name, protocols[name])
	}
	fmt.Fprintf(&report, "Cache reads: hits=%d reported-zero=%d unreported=%d tokens=%d\n", cacheHits, cacheMisses, requests-cacheReported, cacheTokens)
	fmt.Fprintf(&report, "Cache creation: tokens=%d reported=%d unreported=%d\n", cacheCreationTokens, cacheCreationReported, requests-cacheCreationReported)
	fmt.Fprintf(&report, "Input tokens: %d (reported=%d unreported=%d); output tokens: %d (reported=%d unreported=%d)\n", inputTokens, inputReported, requests-inputReported, outputTokens, outputReported, requests-outputReported)
	fmt.Fprintln(&report, "Durations use monotonic clocks. Concurrent/nested stages overlap; do not sum them. Stream/observer samples are per-request aggregates, not per-token samples.")
	fmt.Fprintln(&report, "Percentiles are descriptive. Fewer than 20 samples limit p95/p99; fewer than 100 limit p99. Larger samples still need confidence analysis before a tail-latency claim.")
	columns := tabwriter.NewWriter(&report, 0, 4, 2, ' ', 0)
	fmt.Fprintln(columns, "MODEL\tEFFORT\tRUN STATE\tSTAGE\tOPERATION\tSTAGE STATE\tSAMPLES\tCALLS\tFAILURES\tCANCELED\tp50\tp95\tp99\tCAUTION")
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		group := groups[key]
		sort.Slice(group.durations, func(i, j int) bool { return group.durations[i] < group.durations[j] })
		caution := "-"
		if len(group.durations) < 20 {
			caution = "small n: p95/p99"
		} else if len(group.durations) < 100 {
			caution = "small n: p99"
		}
		fmt.Fprintf(columns, "%s\t%d\t%d\t%d\t%d\t%s\t%s\t%s\t%s\n", key, len(group.durations), group.calls, group.failures, group.canceled, percentile(group.durations, .5), percentile(group.durations, .95), percentile(group.durations, .99), caution)
	}
	if err := columns.Flush(); err != nil {
		return err
	}
	n, err := io.WriteString(writer, report.String())
	if err == nil && n != report.Len() {
		err = io.ErrShortWrite
	}
	if err != nil {
		return fmt.Errorf("timings: write summary: %w", err)
	}
	return nil
}
