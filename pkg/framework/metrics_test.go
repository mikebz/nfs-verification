package framework

import (
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// No NFS server runs on this workstation, so these tests are written against
// bodies in the shape a server serves rather than against a real one. The
// exposition format is a wire format, and what is being protected is the
// reading of it: every wrong answer below is silent. A label set cut in half by
// a comma inside a value reports a series as lost and fails a healthy
// deployment; an error page parsed into zero samples reports a server that is
// publishing normally as one that publishes nothing; a label order that differs
// between two scrapes reports every series as gone.

// serverExposition is a scrape in the shape a server serves one: HELP and TYPE
// lines, a counter with labels, a gauge, a histogram's underlying series, an
// unlabelled series, and a value with a trailing timestamp.
const serverExposition = `# HELP nfsd_rpc_operations_total Operations served.
# TYPE nfsd_rpc_operations_total counter
nfsd_rpc_operations_total{op="READ",export="/export/pvc-1"} 1024
nfsd_rpc_operations_total{op="WRITE",export="/export/pvc-1"} 512
# HELP nfsd_clients Clients holding state.
# TYPE nfsd_clients gauge
nfsd_clients 3
# TYPE nfsd_op_seconds histogram
nfsd_op_seconds_bucket{le="0.1"} 7
nfsd_op_seconds_sum 0.42
nfsd_op_seconds_count 9
# TYPE process_start_time_seconds gauge
process_start_time_seconds 1.7364e+09 1736400000000
`

// TestParseExpositionReadsSeries covers the ordinary body.
//
// Steps:
//  1. Parse a scrape in the shape a server serves one.
//  2. Check the series count, the families, and the types read off TYPE lines.
//  3. Check a labelled value, an unlabelled one, and one with a timestamp.
//  4. Check that nothing in it was unreadable.
func TestParseExpositionReadsSeries(t *testing.T) {
	m := ParseExposition([]byte(serverExposition))

	if m.Unparsed != 0 {
		t.Errorf("%d lines of an ordinary scrape were unreadable, so a healthy endpoint would be "+
			"reported as publishing less than it does: %s", m.Unparsed, m.Describe())
	}
	if got, want := m.Len(), 7; got != want {
		t.Errorf("read %d series, want %d: %s", got, want, m.Describe())
	}
	if got, want := strings.Join(m.Families(), " "), "nfsd_clients nfsd_op_seconds_bucket nfsd_op_seconds_count "+
		"nfsd_op_seconds_sum nfsd_rpc_operations_total process_start_time_seconds"; got != want {
		t.Errorf("families %q, want %q", got, want)
	}
	if got := m.Types["nfsd_rpc_operations_total"]; got != "counter" {
		t.Errorf("the counter's type read as %q, so its direction across a restart would not be checked", got)
	}
	if got := m.Types["nfsd_clients"]; got != "gauge" {
		t.Errorf("the gauge's type read as %q, so a gauge that fell would be reported as a counter reset", got)
	}

	for _, tc := range []struct {
		key  string
		want float64
	}{
		{`nfsd_rpc_operations_total{export="/export/pvc-1",op="READ"}`, 1024},
		{`nfsd_rpc_operations_total{export="/export/pvc-1",op="WRITE"}`, 512},
		{"nfsd_clients", 3},
		{`nfsd_op_seconds_bucket{le="0.1"}`, 7},
		{"process_start_time_seconds", 1.7364e+09},
	} {
		got, ok := m.Samples[tc.key]
		if !ok {
			t.Errorf("no sample under %s; the scrape holds %v", tc.key, sortedKeys(m))
			continue
		}
		if got != tc.want {
			t.Errorf("%s read as %g, want %g", tc.key, got, tc.want)
		}
	}
}

// TestParseExpositionLabelsAreOrderIndependent is the test for the failure with
// no symptom. Nothing requires a server to render a label set in the same order
// twice, and a key built from the order it happened to use would report every
// series as lost the first time it changed, failing a deployment whose metrics
// survived a restart perfectly.
//
// Steps:
//  1. Parse the same series with its labels written in two orders.
//  2. Assert both produce the same key and the same value.
//  3. Parse a label value holding a comma, a brace and an escaped quote, which
//     a split on either character would cut in half.
func TestParseExpositionLabelsAreOrderIndependent(t *testing.T) {
	first := ParseExposition([]byte(`nfsd_ops{op="READ",export="/a",client="10.0.0.1"} 1`))
	second := ParseExposition([]byte(`nfsd_ops{client="10.0.0.1",export="/a",op="READ"} 1`))
	if len(first.Samples) != 1 || len(second.Samples) != 1 {
		t.Fatalf("one series each, got %d and %d", len(first.Samples), len(second.Samples))
	}
	if a, b := sortedKeys(first), sortedKeys(second); a[0] != b[0] {
		t.Errorf("the same series keyed as %q one way round and %q the other, so a server that "+
			"renders its labels in a different order after a restart would report every series lost",
			a[0], b[0])
	}

	awkward := ParseExposition([]byte(`nfsd_ops{path="/a,b}c",note="say \"hi\""} 2`))
	if awkward.Unparsed != 0 || len(awkward.Samples) != 1 {
		t.Fatalf("a label value holding a comma, a brace and an escaped quote was read as %d series "+
			"and %d unreadable lines; a series key cut in half here is reported as a series the "+
			"server stopped publishing", len(awkward.Samples), awkward.Unparsed)
	}
	if got := sortedKeys(awkward)[0]; got != `nfsd_ops{note="say \"hi\"",path="/a,b}c"}` {
		t.Errorf("awkward labels keyed as %q", got)
	}
}

// TestParseExpositionRejectsNonExposition covers the body that is not metrics
// at all.
//
// An error page served with a 200 is the shape that matters: parsed leniently
// it yields no samples, and a caller that reads no samples as "this endpoint
// publishes nothing" files a working server as a deployment defect. The line
// counts are what let the case tell the two apart, so they are asserted here.
//
// Steps:
//  1. Parse an HTML error page and assert it produced no samples, counted its
//     lines as unreadable, and kept an excerpt to show.
//  2. Parse a body of comments alone, which is a live endpoint with nothing on
//     it, and assert it is distinguishable from the page above.
func TestParseExpositionRejectsNonExposition(t *testing.T) {
	page := ParseExposition([]byte("<html>\n<head><title>404 Not Found</title></head>\n<body>nope</body>\n</html>"))
	if page.Len() != 0 {
		t.Errorf("an HTML error page parsed into %d series: %v", page.Len(), sortedKeys(page))
	}
	if page.Unparsed == 0 || page.Unparsed != page.Lines {
		t.Errorf("an HTML error page counted %d unreadable of %d lines; without that count a case "+
			"cannot tell an error page from an endpoint that publishes nothing", page.Unparsed, page.Lines)
	}
	if !strings.Contains(page.Describe(), "404 Not Found") {
		t.Errorf("the description shows nothing of what came back, so the failure would not say why: %s",
			page.Describe())
	}

	empty := ParseExposition([]byte("# HELP nfsd_up whether it is up\n# TYPE nfsd_up gauge\n"))
	if empty.Len() != 0 || empty.Unparsed != 0 || empty.Lines != 0 {
		t.Errorf("a body of comments alone read as %d series, %d unreadable of %d lines",
			empty.Len(), empty.Unparsed, empty.Lines)
	}
}

// TestMetricsEndpointDiscovery covers where a pod says its metrics are.
//
// The two channels are the ones a Prometheus pod service discovery reads, and
// the point of reading exactly those is that a port nobody declared is a port
// no scraper on this cluster would find either. Getting this wrong in the
// permissive direction is the expensive one: an endpoint invented here would
// report a deployment as monitorable when the monitoring an operator runs
// cannot see it.
//
// Steps:
//  1. Discover from the annotations, including path and scheme.
//  2. Discover from a container port named for metrics.
//  3. Assert an explicit scrape=false reads as no endpoint.
//  4. Assert a pod with only the NFS port reads as no endpoint.
func TestMetricsEndpointDiscovery(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pod    *corev1.Pod
		want   MetricsEndpoint
		wantOK bool
	}{
		{
			name: "annotations win",
			pod: serverPodWith(map[string]string{
				scrapeAnnotation: "true", portAnnotation: "9587", pathAnnotation: "/stats/metrics",
				schemeAnnotation: "https",
			}, corev1.ContainerPort{Name: "metrics", ContainerPort: 9100}),
			want:   MetricsEndpoint{Port: 9587, Path: "/stats/metrics", Scheme: "https"},
			wantOK: true,
		},
		{
			name:   "named container port",
			pod:    serverPodWith(nil, corev1.ContainerPort{Name: "http-metrics", ContainerPort: 9587}),
			want:   MetricsEndpoint{Port: 9587, Path: defaultMetricsPath},
			wantOK: true,
		},
		{
			name: "scrape false opts out",
			pod: serverPodWith(map[string]string{scrapeAnnotation: "false", portAnnotation: "9587"},
				corev1.ContainerPort{Name: "metrics", ContainerPort: 9587}),
			wantOK: false,
		},
		{
			name:   "nfs port only",
			pod:    serverPodWith(nil, corev1.ContainerPort{Name: "nfs", ContainerPort: 2049}),
			wantOK: false,
		},
		{
			name: "unparsable port annotation falls through to the named port",
			pod: serverPodWith(map[string]string{portAnnotation: "not-a-number"},
				corev1.ContainerPort{Name: "telemetry", ContainerPort: 9587}),
			want:   MetricsEndpoint{Port: 9587, Path: defaultMetricsPath},
			wantOK: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := MetricsEndpointOf(tc.pod)
			if ok != tc.wantOK {
				t.Fatalf("endpoint found = %v, want %v (%s)", ok, tc.wantOK, DescribePodPorts(tc.pod))
			}
			if !ok {
				return
			}
			if got.Port != tc.want.Port || got.Path != tc.want.Path || got.Scheme != tc.want.Scheme {
				t.Errorf("read port %d path %q scheme %q, want %d %q %q",
					got.Port, got.Path, got.Scheme, tc.want.Port, tc.want.Path, tc.want.Scheme)
			}
			if got.Via == "" {
				t.Error("the endpoint does not say how it was found, so a run cannot record whether " +
					"the deployment announced itself to a scraper or merely named a port")
			}
		})
	}
}

// TestDescribePodPortsNamesWhatWasLooked keeps the failure message honest: the
// case fails a deployment for publishing no metrics, so the message has to show
// what the pod did declare.
//
// The second half is the one that matters. An absent verdict is wrong exactly
// when the deployment marks its scrape target under an annotation this package
// does not read, and a message that printed only the names it already looked
// for would leave the reader no way to notice. Reported as a review finding on
// PR #74.
//
// Steps:
//  1. Describe a pod with no annotations and one NFS port.
//  2. Assert the description names the port and says the annotations are absent.
//  3. Describe a pod carrying an annotation under some other vendor's name, and
//     assert it is shown rather than dropped.
//  4. Assert a value too long to print is bounded rather than reproduced whole.
func TestDescribePodPortsNamesWhatWasLooked(t *testing.T) {
	got := DescribePodPorts(serverPodWith(nil, corev1.ContainerPort{Name: "nfs", ContainerPort: 2049}))
	for _, want := range []string{"no annotations at all", "nfs/2049", "server"} {
		if !strings.Contains(got, want) {
			t.Errorf("the description omits %q, so the failure would not say what was looked at: %s",
				want, got)
		}
	}

	other := map[string]string{
		"monitoring.example.io/port":                       "9100",
		"kubectl.kubernetes.io/last-applied-configuration": strings.Repeat("x", 4096),
	}
	got = DescribePodPorts(serverPodWith(other, corev1.ContainerPort{Name: "nfs", ContainerPort: 2049}))
	if !strings.Contains(got, "monitoring.example.io/port") {
		t.Errorf("an annotation under another vendor's name is dropped, so the evidence that would "+
			"disprove an absent verdict never reaches the reader: %s", got)
	}
	if len(got) > 1024 {
		t.Errorf("an annotation value was reproduced whole, so the failure message is unreadable (%d bytes)",
			len(got))
	}
}

// TestClassifyMetricsVerdicts covers the four verdicts OBS-07 reports.
//
// The routing is the whole assertion, and two of the four are failures that
// name the deployment, so a verdict decided wrongly here either files a working
// server as a defect or passes a deployment whose metrics went dark across a
// failover. It is a pure function so that the decision can be checked without a
// cluster.
//
// Steps:
//  1. No scrape before, which is an endpoint that never published.
//  2. A scrape before and none after, and one after that answered with nothing.
//  3. Both scrapes, with a family missing from the second.
//  4. Both scrapes, with a counter lower afterwards.
//  5. Both scrapes, with the counters carried across.
func TestClassifyMetricsVerdicts(t *testing.T) {
	before := ParseExposition([]byte(serverExposition))
	reset := ParseExposition([]byte(strings.ReplaceAll(serverExposition, "1024", "7")))
	continued := ParseExposition([]byte(strings.ReplaceAll(serverExposition, "1024", "2048")))
	lostFamily := ParseExposition([]byte("# TYPE nfsd_clients gauge\nnfsd_clients 1\n"))
	nothing := ParseExposition([]byte("<html>404</html>"))

	for _, tc := range []struct {
		name          string
		before, after *MetricSet
		want          MetricsVerdict
	}{
		{"no endpoint at all", nil, nil, MetricsAbsent},
		{"endpoint published nothing", &nothing, &before, MetricsAbsent},
		{"did not come back", &before, nil, MetricsNeverResumed},
		{"came back with nothing", &before, &nothing, MetricsNeverResumed},
		{"came back short a family", &before, &lostFamily, MetricsNeverResumed},
		{"counters reset", &before, &reset, MetricsResumedReset},
		{"counters carried across", &before, &continued, MetricsResumedContinuous},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyMetrics(tc.before, tc.after)
			if got.Verdict != tc.want {
				t.Errorf("verdict %s, want %s: %s", got.Verdict, tc.want, got)
			}
		})
	}
}

// TestClassifyMetricsSeparatesFamiliesFromLabels is the deliberate boundary: a
// family that stops being published is the assertion, and a label set that
// changes is not.
//
// Per-client and per-export labels come and go with the clients and exports
// themselves, so a server that comes back before any client has reconnected
// publishes the same families with different labels. Asserting on the exact
// series would report that as metrics lost across the restart, which is a
// healthy deployment failed for a client's timing.
//
// Steps:
//  1. Classify a pair whose family survives with a different client label.
//  2. Assert the verdict passes and the changed series is recorded as a
//     diagnostic rather than as a lost family.
func TestClassifyMetricsSeparatesFamiliesFromLabels(t *testing.T) {
	before := ParseExposition([]byte("# TYPE nfsd_ops counter\nnfsd_ops{client=\"10.0.0.1\"} 5\n"))
	after := ParseExposition([]byte("# TYPE nfsd_ops counter\nnfsd_ops{client=\"10.0.0.2\"} 1\n"))

	got := ClassifyMetrics(&before, &after)
	if got.Verdict != MetricsResumedContinuous {
		t.Errorf("verdict %s, want %s: a client label that changed across the restart is not a "+
			"family the server stopped publishing", got.Verdict, MetricsResumedContinuous)
	}
	if len(got.LostFamilies) != 0 {
		t.Errorf("families reported lost: %v", got.LostFamilies)
	}
	if len(got.LostSeries) != 1 {
		t.Errorf("the series whose labels changed was not recorded as a diagnostic: %s", got)
	}
}

// TestMetricsComparisonTableHoldsBothScrapes checks the artifact, which is the
// only copy: the endpoint is gone by teardown and cannot be asked again.
//
// Steps:
//  1. Classify a pair with a reset counter.
//  2. Assert the table names the verdict, both scrapes and the counter's move.
func TestMetricsComparisonTableHoldsBothScrapes(t *testing.T) {
	before := ParseExposition([]byte(serverExposition))
	after := ParseExposition([]byte(strings.ReplaceAll(serverExposition, "1024", "7")))
	table := ClassifyMetrics(&before, &after).Table()

	for _, want := range []string{string(MetricsResumedReset), "counters reset", "1024 -> 7", "before:", "after:"} {
		if !strings.Contains(table, want) {
			t.Errorf("the table omits %q, so the run keeps no record of what the endpoint said:\n%s",
				want, table)
		}
	}
}

// TestMetricSetDescribeSeparatesNoScrapeFromNoMetrics is the other failure with
// no symptom: Describe is only ever read out of a failure message or the
// artifact bundle, so a wrong sentence here is never noticed by a test, only by
// the person triaging the run it misled. The zero value is what the case holds
// when the endpoint never answered, and described as a real reading it says the
// server was asked and published nothing, which is a different finding about a
// different deployment.
//
// Steps:
//  1. Describe the zero value and assert it says no scrape happened.
//  2. Describe a real scrape of a live endpoint that published nothing, and
//     assert it says so instead, naming the endpoint.
func TestMetricSetDescribeSeparatesNoScrapeFromNoMetrics(t *testing.T) {
	if got := (MetricSet{}).Describe(); got != "no scrape was taken" {
		t.Errorf("a scrape that never happened describes itself as one that did: %q", got)
	}

	live := ParseExposition([]byte("# a live endpoint with nothing on it\n"))
	live.Endpoint = "http://10.0.0.1:9100/metrics"
	got := live.Describe()
	if !strings.Contains(got, live.Endpoint) || strings.Contains(got, "no scrape was taken") {
		t.Errorf("an endpoint that answered with no metrics is not distinguishable from one that was never asked: %q", got)
	}
}

// TestParseExpositionExcerptStaysValidUTF8 guards the quoted body. An error page
// is whatever the server felt like sending, and a byte cut through a multi-byte
// character puts mojibake in the failure message, which reads as the harness
// having mangled the body it is judging.
//
// Steps:
//  1. Parse a non-exposition body, longer than the excerpt bound, whose
//     characters straddle the cut.
//  2. Assert the excerpt was taken and is still valid UTF-8.
func TestParseExpositionExcerptStaysValidUTF8(t *testing.T) {
	body := strings.Repeat("パ", 200)
	m := ParseExposition([]byte(body))

	if m.Excerpt == "" || !strings.HasSuffix(m.Excerpt, "...") {
		t.Fatalf("the body was not excerpted, so the cut is untested: %q", m.Excerpt)
	}
	if !utf8.ValidString(m.Excerpt) {
		t.Errorf("the excerpt was cut mid-character and reads as a harness bug rather than a server body: %q", m.Excerpt)
	}
}

// serverPodWith builds a server pod carrying the given annotations and ports.
func serverPodWith(annotations map[string]string, ports ...corev1.ContainerPort) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "server-0", Namespace: "nfs", Annotations: annotations},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "server", Ports: ports},
		}},
	}
}

// sortedKeys returns the series keys of a scrape, for a message that has to say
// what was actually read.
func sortedKeys(m MetricSet) []string {
	out := make([]string, 0, len(m.Samples))
	for key := range m.Samples {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
