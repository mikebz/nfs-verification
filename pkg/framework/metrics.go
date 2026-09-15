package framework

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// The server's own metrics are the only channel that says anything about NFS
// itself: operation and error rates, clients holding state, locks held, open
// files. The kubelet reports the container and the Kubernetes API reports the
// objects, and neither of them can answer any of those, so a deployment whose
// server publishes nothing leaves its operator watching a process rather than a
// protocol. That boundary is docs/06-observability-design.md section 2, and
// OBS-07 is the case that reads this side of it.
//
// It is read through the API server's pod proxy, on the client the suite
// already holds: an ordinary GET, with no scraper deployed into the cluster and
// no monitoring stack assumed. The kubeconfig needs get on pods/proxy, and a
// refusal is blocked rather than a finding, because nothing was learned about
// the deployment when the suite could not reach the source.
//
// Nothing here knows any metric by name. Which series an NFS server publishes
// is implementation-specific and no document standardizes them, so the case
// compares a scrape against another scrape of the same endpoint rather than
// against a list somebody wrote down.

// metricsReadTimeout bounds one scrape of one endpoint.
//
// Individually bounded for the same reason the kubelet read is: a server that
// has stopped answering must not be able to spend the case's whole budget in
// one call, and the case needs the budget to keep asking.
const metricsReadTimeout = 30 * time.Second

// defaultMetricsPath is where an endpoint serves the exposition format unless
// the pod's annotation says otherwise. It is the Prometheus convention, and the
// only thing that makes an undocumented port scrapable at all.
const defaultMetricsPath = "/metrics"

// The pod annotations a Prometheus deployment is commonly configured to read
// when deciding whether and how to scrape a pod.
//
// The mechanism is documented and the names are not, and the difference is
// worth stating. Kubernetes service discovery exposes every pod annotation as a
// __meta_kubernetes_pod_annotation_<name> meta label, and a scrape config
// selects and routes on it; that is what makes an annotation a discovery
// channel at all. Which annotation is left to the operator, and upstream's own
// example config uses example.io/* placeholders rather than these. The
// prometheus.io names are the convention the common charts and collection
// stacks emit, so they are the best available proxy for "a scraper here would
// find this endpoint", and they are not a guarantee: a deployment marking its
// targets under another name reads as having no endpoint, which is why the
// failure prints every annotation and port the pod does carry.
//
// See docs/06-observability-design.md section 10 for the citations.
const (
	scrapeAnnotation = "prometheus.io/scrape"
	portAnnotation   = "prometheus.io/port"
	pathAnnotation   = "prometheus.io/path"
	schemeAnnotation = "prometheus.io/scheme"
)

// MetricsEndpoint is where a pod says its metrics are.
type MetricsEndpoint struct {
	Namespace string
	Pod       string
	Port      int32
	Path      string
	// Scheme is empty when nothing declared one, which leaves the choice to
	// ScrapeMetrics rather than guessing here.
	Scheme string
	// Via names how the endpoint was found, so a run records whether the
	// deployment announced itself to a scraper or merely named a port.
	Via string
}

func (e MetricsEndpoint) String() string {
	scheme := e.Scheme
	if scheme == "" {
		scheme = "http|https"
	}
	return fmt.Sprintf("%s://%s/%s:%d%s (%s)", scheme, e.Namespace, e.Pod, e.Port, e.Path, e.Via)
}

// MetricsEndpointOf reports where a pod says its metrics are, and whether it
// says so at all.
//
// Two channels are read, in the order a Prometheus pod service discovery reads
// them: the prometheus.io annotations, then a container port named for metrics.
// Both are things the deployment states about itself. Nothing probes ports the
// pod does not declare: a port nobody declared is one no scraper would find, so
// finding it here would report a deployment as monitorable when the monitoring
// an operator actually runs could not see it.
//
// An explicit prometheus.io/scrape of false is an endpoint the deployment has
// opted out of, which is the same answer as having none: a scraper honours it.
func MetricsEndpointOf(p *corev1.Pod) (MetricsEndpoint, bool) {
	e := MetricsEndpoint{Namespace: p.Namespace, Pod: p.Name, Path: defaultMetricsPath}
	if v, ok := p.Annotations[scrapeAnnotation]; ok {
		if scrape, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil && !scrape {
			return MetricsEndpoint{}, false
		}
	}
	if v, ok := p.Annotations[pathAnnotation]; ok && strings.TrimSpace(v) != "" {
		e.Path = strings.TrimSpace(v)
	}
	if v, ok := p.Annotations[schemeAnnotation]; ok {
		if s := strings.ToLower(strings.TrimSpace(v)); s == "http" || s == "https" {
			e.Scheme = s
		}
	}
	if v, ok := p.Annotations[portAnnotation]; ok {
		if port, err := strconv.ParseInt(strings.TrimSpace(v), 10, 32); err == nil && port > 0 && port < 65536 {
			e.Port, e.Via = int32(port), "the "+portAnnotation+" annotation"
			return e, true
		}
	}
	for _, c := range p.Spec.Containers {
		for _, port := range c.Ports {
			if looksLikeMetricsPort(port.Name) {
				e.Port = port.ContainerPort
				e.Via = fmt.Sprintf("container %s port %q", c.Name, port.Name)
				return e, true
			}
		}
	}
	return MetricsEndpoint{}, false
}

// looksLikeMetricsPort reports whether a declared port name says the port
// serves metrics. Port names are capped at 15 characters, so the wordings in
// use are short and few: metrics, http-metrics, telemetry.
func looksLikeMetricsPort(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, "metric") || strings.Contains(n, "telemetry") || strings.Contains(n, "prom")
}

// DescribePodPorts says what a pod declares about being scraped, which is what
// a failure that reports no metrics endpoint has to say next to the claim: "no
// metrics endpoint" is actionable only alongside the ports that were there.
//
// Every annotation is listed, not only the prometheus.io ones this package
// reads. The verdict this message accompanies is that no scraper following the
// convention would find an endpoint, and the way that verdict is wrong is a
// deployment marking its targets under some other name. Printing only the names
// already looked for would hide the one line that disproves the finding. Values
// are bounded because an annotation holds whatever was applied to the object,
// last-applied-configuration included.
func DescribePodPorts(p *corev1.Pod) string {
	var parts []string
	keys := make([]string, 0, len(p.Annotations))
	for key := range p.Annotations {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%q", key, excerpt([]byte(p.Annotations[key]))))
	}
	if len(parts) == 0 {
		parts = append(parts, "no annotations at all")
	}

	var ports []string
	for _, c := range p.Spec.Containers {
		for _, port := range c.Ports {
			name := port.Name
			if name == "" {
				name = "unnamed"
			}
			ports = append(ports, fmt.Sprintf("%s:%s/%d", c.Name, name, port.ContainerPort))
		}
	}
	sort.Strings(ports)
	if len(ports) == 0 {
		ports = append(ports, "no container declares a port")
	}
	return fmt.Sprintf("pod %s/%s carries %s, and declares %s",
		p.Namespace, p.Name, strings.Join(parts, ", "), strings.Join(ports, ", "))
}

// MetricSet is one scrape of one endpoint, keyed so that two scrapes can be
// compared with each other.
type MetricSet struct {
	// At is when the scrape was taken, from the workstation's clock. Nothing
	// is asserted against it; it labels the rows in the bundle.
	At time.Time
	// Endpoint and Scheme record what was read and how it answered, because
	// the second scrape may reach a different pod than the first.
	Endpoint string
	Scheme   string
	// Samples maps a canonical series key, name and sorted labels, to its
	// value. Sorting the labels is what makes two scrapes comparable: nothing
	// requires a server to render a label set in the same order twice, and an
	// order-sensitive key would report every series as lost.
	Samples map[string]float64
	// Types maps a metric family to the word from its # TYPE line. Only a
	// counter has a direction, so a family with no TYPE line is compared for
	// presence and not for whether it went backwards.
	Types map[string]string
	// Lines and Unparsed count what the body held. A body that parsed into no
	// samples is not an endpoint publishing nothing: it is far more often an
	// error page served with a 200, and the counts plus Excerpt are what let a
	// failure say which of the two it was.
	Lines    int
	Unparsed int
	Excerpt  string
}

// Len is the number of series the scrape published.
func (m MetricSet) Len() int { return len(m.Samples) }

// Families returns the metric family names, sorted. A family is the series
// name without its labels, which is the level a dashboard or an alert names.
func (m MetricSet) Families() []string {
	seen := map[string]bool{}
	for key := range m.Samples {
		seen[familyOf(key)] = true
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Describe renders a scrape for a log line or a failure message.
//
// The zero value is a scrape that never happened, and it has to say so. Read as
// an empty-but-real reading it would describe a server that was never asked as
// one that answered with nothing, and that sentence lands in the artifact
// bundle for every never-resumed verdict, which is precisely the case where
// somebody is trying to work out what was and was not observed.
func (m MetricSet) Describe() string {
	if m.Endpoint == "" && m.Len() == 0 && m.Lines == 0 {
		return "no scrape was taken"
	}
	if m.Len() == 0 {
		excerpt := m.Excerpt
		if excerpt == "" {
			excerpt = "an empty body"
		}
		return fmt.Sprintf("%s answered with %d lines, none of which read as the Prometheus exposition "+
			"format, and it begins %q", m.Endpoint, m.Lines, excerpt)
	}
	return fmt.Sprintf("%s published %d series across %d families over %s (%d unreadable lines)",
		m.Endpoint, m.Len(), len(m.Families()), m.Scheme, m.Unparsed)
}

// familyOf strips the labels from a canonical series key.
func familyOf(key string) string {
	if i := strings.IndexByte(key, '{'); i >= 0 {
		return key[:i]
	}
	return key
}

// ScrapeMetrics reads one metrics endpoint through the API server's pod proxy.
//
// A refusal is blocked and never a finding: the suite could not reach the
// source, so nothing was learned about what the server publishes. Every other
// failure is an ordinary error, and it carries what each attempt said, because
// the difference between a connection refused and a 404 is the difference
// between a server that is not serving and a path that is somewhere else.
func ScrapeMetrics(ctx context.Context, c *Client, e MetricsEndpoint) (MetricSet, error) {
	ctx, cancel := context.WithTimeout(ctx, metricsReadTimeout)
	defer cancel()

	schemes := []string{e.Scheme}
	if e.Scheme == "" {
		// The pod named a port and not a scheme. Plain HTTP is the common case
		// and is tried first; a server that serves TLS on that port answers it
		// with a protocol error, and reporting that as an endpoint that did not
		// answer would file a scheme nobody declared as a deployment that
		// publishes nothing.
		schemes = []string{"http", "https"}
	}
	var attempts []string
	for _, scheme := range schemes {
		// The proxy subresource name is "<scheme>:<name>:<port>", not the pod
		// name with something prepended: the apiserver splits it with
		// apimachinery's util/net.SplitSchemeNamePort, whose doc comment gives
		// exactly these three forms. Worth stating because it reads as a
		// name-mangling bug to anyone who has not looked, and it was raised as
		// one in review on PR #74. Checked against a live apiserver, which
		// distinguishes all three cases: "http:<pod>:9999" dials the pod and is
		// refused, "ftp:<pod>:9999" is rejected as an invalid pod request
		// because the scheme is parsed and validated, and "http:no-such-pod"
		// reports pods "no-such-pod" not found, with the scheme stripped.
		raw, err := c.Kube.CoreV1().RESTClient().Get().
			Namespace(e.Namespace).Resource("pods").
			Name(fmt.Sprintf("%s:%s:%d", scheme, e.Pod, e.Port)).
			SubResource("proxy").
			Suffix(pathSegments(e.Path)...).
			Do(ctx).Raw()
		if err != nil {
			if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
				return MetricSet{}, Blockedf("scraping %s was refused: %v. The suite needs get on "+
					"pods/proxy for the server's own metrics, which are the only channel that says "+
					"anything about NFS itself; without it nothing is known about what this "+
					"deployment publishes", e, err)
			}
			attempts = append(attempts, fmt.Sprintf("over %s: %v", scheme, err))
			continue
		}
		set := ParseExposition(raw)
		set.At = time.Now().UTC()
		set.Endpoint = e.String()
		set.Scheme = scheme
		return set, nil
	}
	return MetricSet{}, fmt.Errorf("the metrics endpoint %s did not answer (%s)", e, strings.Join(attempts, "; "))
}

// pathSegments splits a URL path into the segments the request builder joins
// back together. Splitting rather than passing the path whole is what keeps a
// path with more than one segment, /stats/metrics, from being escaped into a
// single segment the server has never heard of.
func pathSegments(path string) []string {
	var out []string
	for _, seg := range strings.Split(path, "/") {
		if seg != "" {
			out = append(out, seg)
		}
	}
	return out
}

// ParseExposition reads a scrape body in the Prometheus text exposition format.
//
// Minimal on purpose: names, labels, values and the TYPE lines, which is all
// any assertion here reads. Histograms and summaries arrive as their underlying
// _bucket, _sum and _count series and are compared as those, since that is what
// they are on the wire. The TYPE line names the parent family rather than any
// of those three, so they are compared for presence and not for direction,
// which is the conservative half: a bucket that went backwards is reported as
// having survived rather than as a reset.
//
// It never returns an error. A body that is not the exposition format at all,
// which is what an error page served with a 200 looks like, parses into no
// samples and a count of the lines it could not read, and the caller reports
// that rather than an endpoint publishing an empty set: the two are told apart
// by whoever reads the run, and only one of them is a defect.
func ParseExposition(raw []byte) MetricSet {
	m := MetricSet{Samples: map[string]float64{}, Types: map[string]string{}}
	m.Excerpt = excerpt(raw)
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			// "# TYPE <family> <type>". Anything else is HELP or a comment.
			if fields := strings.Fields(line); len(fields) >= 4 && fields[1] == "TYPE" {
				m.Types[fields[2]] = strings.ToLower(fields[3])
			}
			continue
		}
		m.Lines++
		key, value, ok := parseSample(line)
		if !ok {
			m.Unparsed++
			continue
		}
		m.Samples[key] = value
	}
	return m
}

// excerpt returns the start of a body, for a message that has to show what came
// back when nothing in it parsed.
func excerpt(raw []byte) string {
	const max = 120
	s := strings.TrimSpace(string(raw))
	s = strings.ReplaceAll(s, "\n", " ")
	// Cut on a rune boundary. The bodies worth quoting here are the ones that
	// are not the exposition format, which means error pages, and those carry
	// whatever the server felt like sending; a byte slice through a multi-byte
	// character puts mojibake in the failure message and makes the reader
	// wonder whether the harness mangled the body it was judging.
	if len(s) > max {
		cut := max
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		return s[:cut] + "..."
	}
	return s
}

// parseSample reads one sample line into a canonical key and its value.
func parseSample(line string) (string, float64, bool) {
	i := strings.IndexAny(line, "{ \t")
	if i <= 0 {
		return "", 0, false
	}
	name := line[:i]
	if !validMetricName(name) {
		return "", 0, false
	}
	rest := line[i:]
	labels := ""
	if strings.HasPrefix(rest, "{") {
		var ok bool
		labels, rest, ok = readLabels(rest)
		if !ok {
			return "", 0, false
		}
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return "", 0, false
	}
	// A trailing timestamp is legal and ignored: the value is compared against
	// another scrape of the same endpoint, and the server's own idea of the
	// time is not what either end of that comparison is stamped with.
	value, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return "", 0, false
	}
	return name + labels, value, true
}

// validMetricName reports whether a token is a metric name. Series lines are
// found by shape rather than by a regexp over the whole line, so this is what
// keeps a stray word in a body that is not exposition format from becoming a
// series with a value.
func validMetricName(name string) bool {
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_', c == ':':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return name != ""
}

// readLabels reads a label set starting at the opening brace and renders it
// canonically: pairs sorted by name, values quoted the same way whatever the
// server did. s must start with "{"; it returns the rendered labels, whatever
// followed the closing brace, and whether the set could be read at all.
//
// The scan is quote-aware because it has to be. A label value may hold a comma
// or a closing brace, and a split on either would cut a series key in half and
// report the series as absent from the next scrape.
func readLabels(s string) (string, string, bool) {
	var pairs []string
	i := 1
	for {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == ',') {
			i++
		}
		if i >= len(s) {
			return "", "", false
		}
		if s[i] == '}' {
			break
		}
		start := i
		for i < len(s) && isLabelNameChar(s[i]) {
			i++
		}
		if i == start {
			return "", "", false
		}
		name := s[start:i]
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		if i >= len(s) || s[i] != '=' {
			return "", "", false
		}
		i++
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		if i >= len(s) || s[i] != '"' {
			return "", "", false
		}
		i++
		var value strings.Builder
		closed := false
		for i < len(s) {
			switch {
			case s[i] == '\\' && i+1 < len(s):
				i++
				switch s[i] {
				case 'n':
					value.WriteByte('\n')
				case '\\':
					value.WriteByte('\\')
				case '"':
					value.WriteByte('"')
				default:
					value.WriteByte('\\')
					value.WriteByte(s[i])
				}
				i++
			case s[i] == '"':
				closed = true
				i++
			default:
				value.WriteByte(s[i])
				i++
			}
			if closed {
				break
			}
		}
		if !closed {
			return "", "", false
		}
		pairs = append(pairs, name+"="+strconv.Quote(value.String()))
	}
	rest := s[i+1:]
	if len(pairs) == 0 {
		return "", rest, true
	}
	sort.Strings(pairs)
	return "{" + strings.Join(pairs, ",") + "}", rest, true
}

func isLabelNameChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}

// MetricsVerdict is what a pair of scrapes taken either side of a restart says
// about the endpoint. The four are mutually exclusive and ordered: absent wins
// over never-resumed, which wins over either resumed verdict.
type MetricsVerdict string

const (
	// MetricsAbsent is a server that published nothing to begin with. It is a
	// failure: a deployment that says nothing about NFS has nothing that could
	// survive a restart, and no container-level signal substitutes for it.
	MetricsAbsent MetricsVerdict = "absent"
	// MetricsNeverResumed is an endpoint that answered before the restart and
	// not after it, or one that came back having lost families it used to
	// publish. Also a failure: every dashboard and every alert built on the
	// missing series goes blind at the moment it is needed most.
	MetricsNeverResumed MetricsVerdict = "never-resumed"
	// MetricsResumedReset is the endpoint back with its counters restarted from
	// a lower value. A pass: a counter reset is what a restarted process is
	// supposed to look like, and every monitoring system knows how to read one.
	MetricsResumedReset MetricsVerdict = "resumed-reset"
	// MetricsResumedContinuous is the endpoint back with its counters carried
	// across. Also a pass: state kept over a restart is not a defect, and the
	// gap in the series that shows the outage is not one either.
	MetricsResumedContinuous MetricsVerdict = "resumed-continuous"
)

// CounterChange is one counter series either side of the restart.
type CounterChange struct {
	Series string
	Before float64
	After  float64
}

// MetricsComparison is what the two scrapes say together.
type MetricsComparison struct {
	Verdict MetricsVerdict
	// Source is what was read, or what was found when there was nothing to
	// read. It is set by the caller rather than derived, because the paths that
	// most need the record are the ones that end before a scrape exists to
	// carry the endpoint: an absent verdict has no MetricSet at all, and the
	// pod is gone by the time anybody reads the bundle.
	Source string
	Before MetricSet
	After  MetricSet

	// LostFamilies are families published before the restart and not after.
	// This is the assertion: a family is what a dashboard names.
	LostFamilies []string
	// NewFamilies are families that only appeared afterwards, reported as a
	// diagnostic. A server that publishes more after a restart than before is
	// not a defect.
	NewFamilies []string
	// LostSeries are series whose family survived but whose exact label set did
	// not. Also a diagnostic, deliberately: per-client and per-export labels
	// come and go with the clients and exports themselves, so asserting on them
	// would fail a healthy server for having no clients reconnected yet.
	LostSeries []string
	// Reset and Continued are the counter series present on both sides, split
	// by which way they moved.
	Reset     []CounterChange
	Continued []CounterChange
}

// ClassifyMetrics turns a pair of scrapes into the verdict OBS-07 reports.
//
// A nil scrape is one that did not happen: nil before is an endpoint that never
// answered, nil after is one that did not come back. Both are passed as nil
// rather than as an empty set so that "the server published nothing" and "the
// suite got nothing" cannot be confused, since only the first is a statement
// about the deployment.
func ClassifyMetrics(before, after *MetricSet) MetricsComparison {
	var c MetricsComparison
	if before == nil || before.Len() == 0 {
		if before != nil {
			c.Before = *before
		}
		if after != nil {
			c.After = *after
		}
		c.Verdict = MetricsAbsent
		return c
	}
	c.Before = *before
	if after == nil || after.Len() == 0 {
		if after != nil {
			c.After = *after
		}
		c.Verdict = MetricsNeverResumed
		return c
	}
	c.After = *after

	beforeFamilies := setOf(before.Families())
	afterFamilies := setOf(after.Families())
	for _, name := range before.Families() {
		if !afterFamilies[name] {
			c.LostFamilies = append(c.LostFamilies, name)
		}
	}
	for _, name := range after.Families() {
		if !beforeFamilies[name] {
			c.NewFamilies = append(c.NewFamilies, name)
		}
	}

	keys := make([]string, 0, len(before.Samples))
	for key := range before.Samples {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		was := before.Samples[key]
		now, ok := after.Samples[key]
		if !ok {
			if afterFamilies[familyOf(key)] {
				c.LostSeries = append(c.LostSeries, key)
			}
			continue
		}
		if before.Types[familyOf(key)] != "counter" {
			continue
		}
		change := CounterChange{Series: key, Before: was, After: now}
		if now < was {
			c.Reset = append(c.Reset, change)
		} else {
			c.Continued = append(c.Continued, change)
		}
	}

	switch {
	case len(c.LostFamilies) > 0:
		c.Verdict = MetricsNeverResumed
	case len(c.Reset) > 0:
		c.Verdict = MetricsResumedReset
	default:
		c.Verdict = MetricsResumedContinuous
	}
	return c
}

func setOf(names []string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, name := range names {
		out[name] = true
	}
	return out
}

// String renders the comparison for a log line or a failure message.
func (c MetricsComparison) String() string {
	return fmt.Sprintf("%s: %d series in %d families before, %d in %d after; %d families lost, "+
		"%d new; %d counters reset, %d continuous, %d series lost within a surviving family",
		c.Verdict, c.Before.Len(), len(c.Before.Families()), c.After.Len(), len(c.After.Families()),
		len(c.LostFamilies), len(c.NewFamilies), len(c.Reset), len(c.Continued), len(c.LostSeries))
}

// Table renders both scrapes into the bundle. Written whether the case passed
// or not: neither endpoint can be asked again once the run is over, and a pass
// where every counter reset is a different run from one where they all carried
// across.
func (c MetricsComparison) Table() string {
	var b strings.Builder
	verdict := string(c.Verdict)
	if verdict == "" {
		// The case exited before it had both scrapes. Saying so beats an empty
		// field, which reads as a verdict somebody forgot to fill in.
		verdict = "not reached: the case ended before it had both scrapes"
	}
	fmt.Fprintf(&b, "verdict: %s\n", verdict)
	if c.Source != "" {
		fmt.Fprintf(&b, "source:  %s\n", c.Source)
	}
	fmt.Fprintf(&b, "before:  %s at %s\n", c.Before.Describe(), stampOf(c.Before))
	fmt.Fprintf(&b, "after:   %s at %s\n", c.After.Describe(), stampOf(c.After))
	writeList(&b, "families lost", c.LostFamilies)
	writeList(&b, "families new", c.NewFamilies)
	writeList(&b, "series lost within a surviving family", c.LostSeries)
	fmt.Fprintf(&b, "\ncounters reset (%d)\n", len(c.Reset))
	for _, ch := range c.Reset {
		fmt.Fprintf(&b, "  %s: %g -> %g\n", ch.Series, ch.Before, ch.After)
	}
	fmt.Fprintf(&b, "\ncounters continuous (%d)\n", len(c.Continued))
	for _, ch := range c.Continued {
		fmt.Fprintf(&b, "  %s: %g -> %g\n", ch.Series, ch.Before, ch.After)
	}
	return b.String()
}

func stampOf(m MetricSet) string {
	if m.At.IsZero() {
		return "no scrape"
	}
	return m.At.UTC().Format(time.RFC3339)
}

func writeList(b *strings.Builder, label string, names []string) {
	fmt.Fprintf(b, "\n%s (%d)\n", label, len(names))
	for _, name := range names {
		fmt.Fprintf(b, "  %s\n", name)
	}
}
