package main

// The corpus: 35 revision families, near-miss distractors, and deterministic
// filler. No randomness anywhere — the filler is enumerated, not sampled, so
// the same corpus comes out of every machine and every Go version.
//
// Two axes are varied on purpose, because a suite that repeats one condition
// measures that condition and not the system:
//
//   - shape: numeric / person / policy / identifier / enum. A fix that only
//     works when the revised value is a number is not a fix.
//   - paraphrased: the newest statement is worded differently from the ones it
//     revises, so the keyword signal points at the STALE fact. That is the
//     hard case and the realistic one — nobody restates a correction using the
//     original wording — and it is reported as its own stratum.

type family struct {
	ID          string
	Statements  []string // oldest first; only the last one is true
	Query       string
	Correct     string   // substring identifying the current value
	Stale       []string // substrings identifying the retired values
	Shape       string
	Paraphrased bool
}

var families = []family{
	{"R01", []string{
		"the webhook retry limit is 3 attempts",
		"the webhook retry limit was raised to 5 attempts",
		"the webhook retry limit is now 8 attempts"},
		"how many webhook retries do we allow?", "8 attempts",
		[]string{"3 attempts", "5 attempts"}, "numeric", false},
	{"R02", []string{
		"the export cap is 10000 rows",
		"the export cap was raised to 50000 rows"},
		"what is the maximum export size?", "50000 rows",
		[]string{"10000 rows"}, "numeric", false},
	{"R03", []string{
		"Ana Duarte is on-call for billing",
		"Kenji Mori took over on-call for billing"},
		"who is on call for billing?", "Kenji Mori",
		[]string{"Ana Duarte"}, "person", false},
	{"R04", []string{
		"deploys are frozen on Fridays",
		"we ship any weekday now, the end-of-week block is gone"},
		"can we deploy on a Friday?", "any weekday",
		[]string{"frozen on Fridays"}, "policy", true},
	{"R05", []string{
		"the session timeout is 30 minutes",
		"the session timeout was shortened to 22 minutes",
		"the session timeout is 10 minutes after the security review"},
		"how long until a session times out?", "10 minutes",
		[]string{"30 minutes", "22 minutes"}, "numeric", false},
	{"R06", []string{
		"the public API allows 100 requests per minute",
		"throttling was relaxed to 250 requests per minute"},
		"what is the public API rate limit?", "250 requests",
		[]string{"100 requests"}, "numeric", true},
	{"R07", []string{
		"the primary database lives in eu-west-1",
		"we migrated the primary database to eu-central-1"},
		"which region hosts the primary database?", "eu-central-1",
		[]string{"eu-west-1"}, "identifier", false},
	{"R08", []string{
		"backups are retained for 14 days",
		"backup retention was extended to 91 days"},
		"how long do we keep backups?", "91 days",
		[]string{"14 days"}, "numeric", false},
	{"R09", []string{
		"single sign-on runs through Okta",
		"identity moved off Okta; Keycloak handles single sign-on"},
		"what do we use for SSO?", "Keycloak",
		[]string{"through Okta"}, "identifier", true},
	{"R10", []string{
		"the upload limit is 25 megabytes",
		"large-file support landed: the ceiling is 512 megabytes"},
		"how big can an uploaded file be?", "512 megabytes",
		[]string{"25 megabytes"}, "numeric", true},
	{"R11", []string{
		"CI runs on ubuntu-20.04 runners",
		"the runner image was bumped to ubuntu-24.04"},
		"which runner image does CI use?", "ubuntu-24.04",
		[]string{"ubuntu-20.04"}, "identifier", false},
	{"R12", []string{
		"passwords must be at least 8 characters",
		"the password floor moved to 12 characters",
		"minimum password length is 16 characters"},
		"what is the minimum password length?", "16 characters",
		[]string{"8 characters", "12 characters"}, "numeric", false},
	{"R13", []string{
		"Marco Bianchi leads the search team",
		"Priya Raman has taken over search"},
		"who leads the search team?", "Priya Raman",
		[]string{"Marco Bianchi"}, "person", true},
	{"R14", []string{
		"feature flags are served by LaunchDarkly",
		"we self-host Unleash for feature flags now"},
		"which feature flag service do we run?", "Unleash",
		[]string{"LaunchDarkly"}, "identifier", true},
	{"R15", []string{
		"the cache TTL is 60 seconds",
		"cache entries now live 900 seconds"},
		"how long does a cache entry live?", "900 seconds",
		[]string{"60 seconds"}, "numeric", true},
	{"R16", []string{
		"production logs at debug level",
		"log verbosity in production was cut to warn level"},
		"what log level does production use?", "warn level",
		[]string{"debug level"}, "enum", true},
	{"R17", []string{
		"clients target API version v2",
		"all clients were moved to API version v3"},
		"which API version do clients call?", "version v3",
		[]string{"version v2"}, "identifier", false},
	{"R18", []string{
		"the pager rotation lasts one week",
		"rotations were stretched to a fortnight"},
		"how long is a pager rotation?", "fortnight",
		[]string{"one week"}, "enum", true},
	{"R19", []string{
		"the coverage gate is 60 percent",
		"the coverage gate rose to 75 percent",
		"the coverage gate is 85 percent"},
		"what is the test coverage gate?", "85 percent",
		[]string{"60 percent", "75 percent"}, "numeric", false},
	{"R20", []string{
		"containers build from an alpine base image",
		"we rebased every image onto distroless"},
		"what base image do containers use?", "distroless",
		[]string{"alpine base"}, "identifier", true},
	{"R21", []string{
		"events flow through RabbitMQ",
		"the event bus was replaced by NATS"},
		"what message broker do we use?", "NATS",
		[]string{"RabbitMQ"}, "identifier", true},
	{"R22", []string{
		"the minimum accepted TLS version is 1.2",
		"TLS 1.3 is now the floor for every endpoint"},
		"what is the minimum TLS version?", "TLS 1.3",
		[]string{"TLS version is 1.2"}, "identifier", true},
	{"R23", []string{
		"staging runs on a db.t3.medium instance",
		"staging was resized to db.r6g.large"},
		"what instance type does staging use?", "db.r6g.large",
		[]string{"db.t3.medium"}, "identifier", false},
	{"R24", []string{
		"we cut a release once a month",
		"releases go out weekly since the pipeline rewrite"},
		"how often do we release?", "weekly since",
		[]string{"once a month"}, "enum", true},
	{"R25", []string{
		"a sev1 must be acknowledged within 17 minutes",
		"the sev1 acknowledgement target is 4 minutes"},
		"how fast must a sev1 be acknowledged?", "4 minutes",
		[]string{"17 minutes"}, "numeric", false},
	{"R26", []string{
		"Sofia Lindqvist owns the billing runbook",
		"billing runbook ownership passed to Tomas Nowak"},
		"who owns the billing runbook?", "Tomas Nowak",
		[]string{"Sofia Lindqvist"}, "person", false},
	{"R27", []string{
		"the search index is rebuilt nightly",
		"reindexing dropped to a weekend job"},
		"how often is the search index rebuilt?", "weekend job",
		[]string{"rebuilt nightly"}, "enum", true},
	{"R28", []string{
		"we deploy with Helm charts",
		"deployment moved to Kustomize overlays"},
		"how do we deploy to Kubernetes?", "Kustomize",
		[]string{"Helm charts"}, "identifier", true},
	{"R29", []string{
		"the on-call handover happens at 09:00 UTC",
		"handover was moved to 13:30 UTC"},
		"when is the on-call handover?", "13:30 UTC",
		[]string{"09:00 UTC"}, "numeric", false},
	{"R30", []string{
		"merge requires one approving review",
		"two approving reviews are required to merge"},
		"how many approvals does a merge need?", "two approving",
		[]string{"one approving"}, "numeric", false},
	{"R31", []string{
		"secrets live in HashiCorp Vault",
		"secret storage was consolidated into AWS Secrets Manager"},
		"where are secrets stored?", "Secrets Manager",
		[]string{"HashiCorp Vault"}, "identifier", true},
	{"R32", []string{
		"the mobile app targets Android 11",
		"the minimum supported Android release is now Android 14"},
		"what Android version does the app support?", "Android 14",
		[]string{"Android 11"}, "identifier", false},
	{"R33", []string{
		"Omar Haddad reviews all schema migrations",
		"schema migration review is now Nadia Petrova's call"},
		"who reviews schema migrations?", "Nadia Petrova",
		[]string{"Omar Haddad"}, "person", true},
	{"R34", []string{
		"error budgets are computed monthly",
		"the error budget window shrank to a rolling 28 days"},
		"over what window is the error budget computed?", "rolling 28",
		[]string{"computed monthly"}, "enum", true},
	{"R35", []string{
		"the CDN is Fastly",
		"we serve static assets through Cloudflare",
		"Bunny CDN took over static asset delivery"},
		"which CDN serves static assets?", "Bunny CDN",
		[]string{"is Fastly", "through Cloudflare"}, "identifier", true},
}

// nearMiss shares a subject with a probe family but answers a different
// question. Unrelated filler would leave keyword retrieval trivial.
var nearMiss = []string{
	"the webhook signing secret rotates every quarter",
	"export jobs run on the analytics replica",
	"billing reconciliation runs at 02:00 UTC",
	"deploys require a green smoke suite",
	"session cookies are marked SameSite=Lax",
	"the public API is documented with OpenAPI 3.1",
	"the primary database runs PostgreSQL 16",
	"backups are encrypted with a per-tenant key",
	"single sign-on sessions federate to the mobile app",
	"uploads are virus-scanned before storage",
	"CI caches Go modules between runs",
	"passwords are hashed with Argon2id",
	"the search team owns the relevance dashboard",
	"feature flags are evaluated server-side only",
	"cache warming runs after every deploy",
	"production logs ship to the central collector",
	"API clients must send an idempotency key",
	"the pager escalates to the secondary after ten minutes",
	"coverage reports are published as CI artifacts",
	"container images are signed with cosign",
	"the event bus retains messages for seven days",
	"TLS certificates renew automatically",
	"staging data is anonymised nightly",
	"release notes are generated from commit trailers",
	"sev2 incidents do not page overnight",
	"the billing runbook lives in the internal wiki",
	"search queries are logged without user identifiers",
	"Kubernetes namespaces map one-to-one to teams",
	"on-call compensation is paid per rotation",
	"merge queues serialise integration runs",
	"secret rotation is audited every quarter",
	"the mobile app ships through the closed beta track",
	"schema migrations run inside a transaction",
	"error budget burn alerts fire at 2x rate",
	"static assets carry a one-year cache header",
}

var (
	fillServices = []string{"catalog", "billing", "identity", "notifications",
		"shipping", "pricing", "search", "inventory", "reporting", "gateway"}
	fillPeople = []string{"Luis Ferreira", "Yuki Tanaka", "Ines Correia",
		"Dmitri Volkov", "Aisha Bello"}
)
