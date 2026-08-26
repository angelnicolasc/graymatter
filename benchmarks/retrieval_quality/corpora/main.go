// Command corpora generates the multilingual-es and long-horizon fixture
// sets deterministically: same seed, byte-identical output. The generated
// JSONL is committed, so CI never needs to run this; the generator exists so
// every fact and query in the committed files can be traced to a rule rather
// than to a hand edit.
//
// Usage: go run ./benchmarks/retrieval_quality/corpora -out ../../benchmarks/fixtures
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
)

type fact struct {
	ID      string `json:"id"`
	Session int    `json:"session"`
	Domain  string `json:"domain"`
	Kind    string `json:"kind"`
	Text    string `json:"text"`
}

type query struct {
	ID        string   `json:"id"`
	Domain    string   `json:"domain"`
	AskedAt   int      `json:"asked_at_session"`
	Text      string   `json:"text"`
	Gold      []string `json:"gold"`
	Forbidden []string `json:"forbidden,omitempty"`
}

func main() {
	out := flag.String("out", "../../benchmarks/fixtures", "directory that receives one sub-directory per corpus")
	flag.Parse()

	if err := writeMultilingualES(filepath.Join(*out, "multilingual-es")); err != nil {
		fmt.Fprintf(os.Stderr, "multilingual-es: %v\n", err)
		os.Exit(1)
	}
	if err := writeLongHorizon(filepath.Join(*out, "long-horizon")); err != nil {
		fmt.Fprintf(os.Stderr, "long-horizon: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("corpora written")
}

func writeJSONL(path string, rows []any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// multilingual-es
//
// Spanish commercial/support sessions. Every fact carries ASCII anchors
// (product names, order ids, emails) inside otherwise accented Spanish, so
// the corpus separates what the ASCII-only tokenizer can reach from what it
// cannot. Queries are declared as one of two classes at authoring time:
//
//	es-ascii-NN  — gold shares an explicit ASCII anchor with the query
//	es-puro-NN   — matching requires accented/Spanish-only tokens
// ---------------------------------------------------------------------------

var esNames = []string{"María", "Jorge", "Lucía", "Andrés", "Camila", "Rodrigo", "Valentina", "Esteban"}
var esCompanies = []string{"Grupo Ávila", "Talleres Ibáñez", "Clínica Nogal", "Logística Céspedes", "Editorial Bruma", "Cafetal Ortega"}

type esAnchor struct {
	kind string // product | order | email | plan
	val  string
}

var esAnchors = []esAnchor{
	{"producto", "Polar"},
	{"producto", "Nexus"},
	{"pedido", "ORD-4471"},
	{"pedido", "ORD-9083"},
	{"email", "soporte@avila.example"},
	{"email", "compras@ibanez.example"},
	{"plan", "Plan Ándes"}, // deliberately accented: NOT an ASCII anchor
	{"plan", "Plan Delta"},
}

func writeMultilingualES(dir string) error {
	rng := rand.New(rand.NewSource(20260826))
	var facts []fact
	var queries []query
	usedText := map[string]bool{}
	goldByAnchor := map[string]string{} // anchor value -> fact id carrying it

	sessions := 15
	n := 0
	for s := 1; s <= sessions; s++ {
		for d := 0; d < 10; d++ { // 150 facts total
			name := esNames[rng.Intn(len(esNames))]
			company := esCompanies[rng.Intn(len(esCompanies))]
			a := esAnchors[rng.Intn(len(esAnchors))]

			var text string
			switch a.kind {
			case "producto":
				text = fmt.Sprintf("%s de %s confirmó que la integración con %s va con retraso de una semana por el cambio de proveedor.", name, company, a.val)
			case "pedido":
				text = fmt.Sprintf("El pedido %s de %s se reenvió tras la reclamación de %s; el anterior llegó dañado.", a.val, company, name)
			case "email":
				text = fmt.Sprintf("%s pidió que toda la correspondencia de %s vaya al correo %s y no al teléfono.", name, company, a.val)
			case "plan":
				text = fmt.Sprintf("Renovación acordada con %s (%s): permanece en el %s hasta diciembre sin aumento.", company, name, a.val)
			}
			if usedText[text] {
				continue
			}
			usedText[text] = true
			n++
			id := fmt.Sprintf("es-%03d", n)
			domain := []string{"ventas", "soporte", "facturacion", "producto"}[d%4]
			facts = append(facts, fact{ID: id, Session: s, Domain: domain, Kind: a.kind, Text: text})
			if _, seen := goldByAnchor[a.val]; !seen || rng.Intn(2) == 0 {
				goldByAnchor[a.val] = id
			}
		}
	}

	// --- ascii-anchor class -------------------------------------------------
	type qa struct {
		id, domain, q, anchor string
	}
	asciiQs := []qa{
		{"es-ascii-01", "producto", "¿Cómo va la integración con Polar?", "Polar"},
		{"es-ascii-02", "producto", "Estado de la integración Nexus", "Nexus"},
		{"es-ascii-03", "soporte", "Qué pasó con el pedido ORD-4471", "ORD-4471"},
		{"es-ascii-04", "soporte", "Reclamación del pedido ORD-9083", "ORD-9083"},
		{"es-ascii-05", "facturacion", "Correo de facturación de Grupo Ávila", "soporte@avila.example"},
		{"es-ascii-06", "ventas", "A qué email contactar a Talleres Ibáñez", "compras@ibanez.example"},
		{"es-ascii-07", "facturacion", "Renovación Plan Delta sin aumento", "Plan Delta"},
		{"es-ascii-08", "producto", "Retraso de la integración Polar", "Polar"},
		{"es-ascii-09", "soporte", "Pedido reenviado ORD-4471", "ORD-4471"},
		{"es-ascii-10", "ventas", "Contacto email de Logística Céspedes", "soporte@avila.example"},
	}
	for i, q := range asciiQs {
		gold, ok := goldByAnchor[q.anchor]
		if !ok {
			return fmt.Errorf("anchor %q has no fact", q.anchor)
		}
		queries = append(queries, query{
			ID: q.id, Domain: q.domain, AskedAt: sessions + 1,
			Text: q.q, Gold: []string{gold},
		})
		_ = i
	}

	// --- pure-es class -------------------------------------------------------
	// Gold facts are chosen from memory-bearing sentences whose distinguishing
	// tokens are accented or Spanish verb stems the ASCII tokenizer drops.
	pureQs := []struct {
		id, domain, q string
		match         func(f fact) bool
	}{
		{"es-puro-01", "soporte", "Quién pidió que no lo llamen por teléfono",
			func(f fact) bool { return f.Kind == "email" }},
		{"es-puro-02", "facturacion", "Renovaciones acordadas sin aumento hasta diciembre",
			func(f fact) bool { return f.Kind == "plan" && len(f.Text) > 40 }},
		{"es-puro-03", "soporte", "Pedidos reenviados después de una reclamación",
			func(f fact) bool { return f.Kind == "pedido" }},
		{"es-puro-04", "ventas", "Confirmaciones de integración con retraso de una semana",
			func(f fact) bool { return f.Kind == "producto" }},
		{"es-puro-05", "facturacion", "Correspondencia que debe ir por correo electrónico",
			func(f fact) bool { return f.Kind == "email" }},
	}
	for i, pq := range pureQs {
		// Pick the earliest matching fact as gold; the rest are distractors of
		// the same kind (recent ones are what the window would return).
		var goldID string
		for _, f := range facts {
			if pq.match(f) {
				goldID = f.ID
				break
			}
		}
		if goldID == "" {
			return fmt.Errorf("query %s has no gold", pq.id)
		}
		_ = i
		queries = append(queries, query{
			ID: pq.id, Domain: pq.domain, AskedAt: sessions + 1,
			Text: pq.q, Gold: []string{goldID},
		})
	}

	if len(queries) < 5 {
		return fmt.Errorf("only %d queries generated", len(queries))
	}

	rows := make([]any, len(facts))
	for i, f := range facts {
		rows[i] = f
	}
	if err := writeJSONL(filepath.Join(dir, "corpus-v1.jsonl"), rows); err != nil {
		return err
	}
	qrows := make([]any, len(queries))
	for i, q := range queries {
		qrows[i] = q
	}
	return writeJSONL(filepath.Join(dir, "queries-v1.jsonl"), qrows)
}

// ---------------------------------------------------------------------------
// long-horizon
//
// Fifty sessions of ordinary engineering traffic. Decisions are planted in
// sessions 1–5 and again as superseded variants near the end, so returning
// the right answer means reaching across the whole timeline instead of
// surfacing the newest similar-sounding thing.
// ---------------------------------------------------------------------------

var lhDomains = []string{"deploy", "billing", "incidents", "people", "roadmap"}

type lhSeed struct {
	domain string
	early  string // decision planted early (gold)
	late   string // newer variant, superseded by the early one's successor
	query  string
}

func writeLongHorizon(dir string) error {
	rng := rand.New(rand.NewSource(19700101))
	const sessions = 50
	var facts []fact
	var queries []query
	usedText := map[string]bool{}

	seeds := []lhSeed{
		{"deploy",
			"We decided deploys roll out canary-first: 5% for one hour before full rollout.",
			"Deploy process discussion: someone proposed rolling everything out at once.",
			"What did we decide about canary deployments?"},
		{"billing",
			"Billing stays on Polar after the migration review; Stripe integration work is cancelled.",
			"Billing platform options were compared during the pricing review.",
			"Which billing provider did we settle on?"},
		{"incidents",
			"The pager escalation policy changed: page the on-call directly, skip the manager hop.",
			"Incident response meeting covered paging paths and escalation steps.",
			"How does pager escalation work now?"},
		{"people",
			"Dana owns the vendor-review rota from Q3 onwards; route vendor questions to her.",
			"Team roles were shuffled during the planning session.",
			"Who owns the vendor-review rota?"},
		{"roadmap",
			"The offline-sync milestone was pushed to next year; mobile polish takes its slot.",
			"Roadmap planning session discussed offline-sync and mobile priorities.",
			"What happened to the offline-sync milestone?"},
		{"deploy",
			"Rollback authority sits with the releasing engineer alone; no committee needed.",
			"Postmortem action items mentioned clarifying rollback permissions.",
			"Who can authorize a rollback?"},
		{"billing",
			"Invoice disputes go through support triage first, never straight to finance.",
			"Finance workflow notes mentioned invoice handling changes.",
			"Where do invoice disputes go?"},
		{"people",
			"Interview loops cap at four stages; any more needs a written exception.",
			"Hiring process review talked about interview stage counts.",
			"How many interview stages are allowed?"},
	}

	// Plant each early decision twice in sessions 1-3 (repetition makes it
	// established), scatter the late variants in sessions 40-49, then fill the
	// middle with ordinary traffic.
	n := 0
	plant := func(session int, text, domain, kind string) string {
		if usedText[text] {
			return ""
		}
		usedText[text] = true
		n++
		id := fmt.Sprintf("lh-%04d", n)
		facts = append(facts, fact{ID: id, Session: session, Domain: domain, Kind: kind, Text: text})
		return id
	}

	type goldPair struct{ earlyIDs []string }
	golds := map[int]goldPair{} // seed index -> early fact ids

	for si, s := range seeds {
		var ids []string
		for _, sess := range []int{1, 2, 3} {
			variants := []string{
				s.early,
				fmt.Sprintf("Decision recorded: %s", lowerFirst(s.early)),
				fmt.Sprintf("%s (owner follow-up scheduled).", s.early),
			}
			id := plant(sess+si%2, variants[si%len(variants)], s.domain, "decision")
			if id != "" {
				ids = append(ids, id)
			}
		}
		golds[si] = goldPair{earlyIDs: ids}
	}
	for si, s := range seeds {
		sess := 40 + si
		late := plant(sess, fmt.Sprintf("%s This line was later revised; see the original decision.", s.late), s.domain, "variant")
		if late != "" {
			// Mark late variants forbidden for their query.
		}
	}

	// Ordinary filler traffic for the remaining sessions.
	fillers := []string{
		"Stood up the staging refresh cron; runs nightly at 02:00 UTC.",
		"Customer call: %s asked about SSO availability on the mid-tier plan.",
		"Flaky test quarantined in the checkout flow pending investigation.",
		"Onboarding notes: new engineer paired with the payments team today.",
		"Azure peering latency graphs reviewed; nothing actionable this week.",
		"Docs pass on the quickstart finished; screenshots still outdated.",
		"Sprint review moved to Thursday 15:00 permanently.",
		"Support backlog burned down below fifty tickets for the first time this quarter.",
		"Security training deadline set for the end of the month.",
		"Vendor contract renewal reminder filed for the analytics pipeline.",
	}
	companies := []string{"Northwind", "Contoso Retail", "Globex Logistics"}
	for s := 6; s <= sessions; s++ {
		for k := 0; k < 9; k++ {
			text := fillers[rng.Intn(len(fillers))]
			if containsPlaceholder(text) {
				text = fmt.Sprintf(text, companies[rng.Intn(len(companies))])
			}
			if usedText[text] {
				text = fmt.Sprintf("%s (session %d note %d)", trimDot(text), s, k)
			}
			plant(s, text, lhDomains[rng.Intn(len(lhDomains))], "note")
		}
	}

	for si, s := range seeds {
		q := query{
			ID:      fmt.Sprintf("lh-q-%02d", si+1),
			Domain:  s.domain,
			AskedAt: sessions,
			Text:    s.query,
			Gold:    append([]string{}, golds[si].earlyIDs...),
		}
		// Forbid the late variant lines with matching domain so DeadRate has
		// teeth: returning the newest similar thing is the failure mode here.
		for _, f := range facts {
			if f.Session >= 40 && f.Domain == s.domain && f.Kind == "variant" {
				q.Forbidden = append(q.Forbidden, f.ID)
			}
		}
		queries = append(queries, q)
	}

	rows := make([]any, len(facts))
	for i, f := range facts {
		rows[i] = f
	}
	if err := writeJSONL(filepath.Join(dir, "corpus-v1.jsonl"), rows); err != nil {
		return err
	}
	qrows := make([]any, len(queries))
	for i, q := range queries {
		qrows[i] = q
	}
	return writeJSONL(filepath.Join(dir, "queries-v1.jsonl"), qrows)
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	b := []byte(s)
	if b[0] >= 'A' && b[0] <= 'Z' {
		b[0] += 'a' - 'A'
	}
	return string(b)
}

func trimDot(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '.') {
		s = s[:len(s)-1]
	}
	return s
}

// containsPlaceholder reports whether s holds a %s verb left by the filler pool.
func containsPlaceholder(s string) bool {
	for i := 0; i+1 < len(s); i++ {
		if s[i] == '%' && s[i+1] == 's' {
			return true
		}
	}
	return false
}
