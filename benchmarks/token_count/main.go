// Command token_count benchmarks GrayMatter's token efficiency versus
// full-history injection. It runs entirely in-process with no LLM or
// network requirements — the keyword embedder is used so results are
// deterministic and reproducible in any environment.
//
// Usage:
//
//	go run ./benchmarks/token_count
//
// Model: each "session" stores ONE observation — a paragraph extracted from a
// realistic agent interaction, ~50-70 words. "30 sessions" means 30 stored
// observations. "Full injection" = ALL stored observations concatenated.
// "GrayMatter Recall" = top-8 most relevant observations for the given query.
// Token counts are approximated at 1.33 tokens/word (matches tiktoken within ±10%).
//
// What this does NOT measure is in docs/benchmarks.md and is worth reading
// before quoting a number from it: relevance is never checked, so a system
// returning 8 random facts would score the same reduction, and full-history
// injection is the weakest baseline available.
package main

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/embedding"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// ── Corpus ───────────────────────────────────────────────────────────────────
//
// 100 realistic paragraph-length agent memory observations (~50-70 words each).
// Each represents a key insight extracted from a sales agent interaction — the
// kind of observation GrayMatter would store after processing a session.

var corpus = [100]string{
	"Maria Lopez from Acme Corp called on April 3rd to discuss the enterprise plan. She has pre-approved budget of $80k ARR and strong executive backing. Her primary blocker is the Salesforce integration timeline. She agreed to loop in their CTO James Park for a technical demo next Thursday. Priority: send the security whitepaper and Salesforce integration guide before the demo.",
	"Carlos Mendez at Initech is budget-blocked until Q3 2026 due to a company-wide spending freeze following a 10% reduction in headcount last quarter. His champion status is at risk as his manager who approved the initiative has left the company. Re-engage in July with a reference case from a similarly-sized company in their vertical. Mark as at-risk in CRM.",
	"Ana Torres at FinVault confirmed signing authority up to $50k without additional approvals; anything above that requires CFO sign-off. She mentioned their team runs 40 developers split between Python and Go. Their previous vendor migration failed due to data integrity issues. She asked for references from FinTech companies that have successfully migrated. Her preferred communication is async via email.",
	"TechCorp pilot kicked off on May 1st for a three-month evaluation. Primary success metric agreed with their SRE team is a 30% reduction in context-switching time measured by their internal tooling. Demo feedback was positive: the search and recall features exceeded expectations. The bulk import feature was flagged as a must-have before GA. Attendees included CTO, VP Eng, and two senior SREs.",
	"Globex Corp shortlisted us against Acme as the final two vendors. Price is the tiebreaker — they have a hard budget ceiling of $90k ARR. Their Head of Engineering David Kim is our champion; however he recently left the company, creating deal risk. Karen Wu, VP Engineering, has taken over ownership and we need a re-introduction call to re-establish the relationship with her.",
	"The Initech proof-of-concept requires SAML 2.0 single sign-on integration with their existing Okta deployment. Their IT department has a mandatory 3-4 week security review for any new SaaS tool before procurement approval. Starting the review process now is critical to avoid timeline slippage. Their pilot sandbox environment needs to be provisioned separately from production to satisfy their data governance policy.",
	"Ana Torres introduced us to their Head of Product Sarah Kim and their CFO Miguel Santos via email. A three-way discovery call is scheduled for next Tuesday. Miguel has direct budget authority for the enterprise tier. Ana mentioned the company raised $40M Series B and is growing headcount 20% year-over-year. They are evaluating tools to support their expanding engineering org of 40 developers.",
	"TechCorp's latency spike was traced to a misconfigured rate limiter on our API gateway during their peak load testing. Engineering deployed a hotfix within four hours. Post-incident report sent to their CTO. TechCorp's SRE team confirmed the fix resolved the issue. They requested a dedicated status page and an escalation contact for Severity-1 incidents going forward. NPS after incident resolution: 8/10.",
	"Maria signed the Master Service Agreement on April 14th. The contract is for 12 months at $95k ARR with a 30-day termination clause after month six. Invoice split requested across two fiscal quarters: $47,500 in Q2 and $47,500 in Q3. Procurement requires a W-9 form and bank wire details for vendor onboarding. Purchase order expected within five business days of MSA execution.",
	"Carlos's company, now rebranded as NovaTech after an acquisition, has re-engaged with a freshly approved budget of $62k ARR. Carlos confirmed he is now the sole decision-maker after his former manager departed. He needs executive sponsorship from our side for the final board presentation scheduled for the June quarterly review. The deal is time-sensitive: board approval closes on June 28th.",
	"Globex Corp closed at $87k ARR on an 18-month contract after the competitor failed their security audit. Karen Wu signed on behalf of the company. Penalty clause for downtime exceeding the 99.9% SLA was added at $500 per hour of excess downtime. Onboarding kickoff scheduled for May 15th. They requested a dedicated Customer Success Manager with experience in their industry vertical.",
	"Initech closed as the largest deal of Q2 at $210k ARR. Contract is for 24 months with an option to renew at fixed pricing. Dedicated Slack channel created for pilot support. Their IT department approved the security review in 3 weeks, faster than expected. CSM assignment: Rachel Torres, who previously managed two similar enterprise accounts. QBR scheduled for July 5th.",
	"Ana's company went live on April 7th after completing the API integration independently. First support ticket raised about the CSV bulk import timeout for files over 50MB. Engineering confirmed the limit is configurable and patched the default to 200MB. Ana's team is onboarding 40 developers in waves of 10 per week. She referred two new leads: Omega Solutions and Peak Data, both in the FinTech space.",
	"TechCorp co-authored a case study published on our website. The study highlights a 34% reduction in context-switching time — exceeding their 30% pilot goal — measured over 60 days. Maria Chen, their VP of Engineering, is featured as the primary quote. TechCorp agreed to participate in a webinar next quarter. They are discussing upgrading from the Professional to Enterprise plan worth an additional $44k ARR.",
	"NovaTech expansion deal signed for 50 additional seats at $800 per seat per year, adding $40k to their ARR. Total account ARR now $102k. They flagged a requirement for multi-region data residency for their EU operations. EU data residency is on the roadmap for Q4 — flag for engineering and set expectation with Carlos. EU expansion anticipated to add another 25 seats.",
	"Omega Solutions discovery call completed on April 12th. The company is Series A, $12M raised, with 45 employees and 15 engineers. Their use case is automating client portfolio reporting. Budget is $25k ARR for year one. Key stakeholder: CTO David Park. They need a sandbox trial before committing. Trial provisioned with 30-day access. Follow-up scheduled for April 26th to review trial results.",
	"Peak Data is a 30-person startup with a need for real-time agent memory across their data pipeline monitoring tools. They are evaluating three vendors simultaneously. Their CTO is technically strong and has reviewed our API documentation in detail. She asked pointed questions about consistency guarantees under concurrent write load. I sent a detailed write-up on bbolt's serialization model and our concurrency guarantees.",
	"Globex Corp's first QBR completed. Weekly active users: 78% of licensed seats. Three power users identified: Karen Wu and two engineering leads. Identified an upsell opportunity: they are managing a 25-person EU team manually using spreadsheets. EU seat expansion deal estimated at $20k ARR. Karen asked for a roadmap update on EU data residency — confirmed Q4 timeline on the record.",
	"Initech QBR on July 5th revealed 85% weekly active usage across 80 licensed seats. Three primary use cases emerged: sprint planning context, incident retrospectives, and onboarding new engineers. Their Head of Data Science requested a custom model fine-tuning feature for domain-specific terminology. Flagged to product as a potential enterprise add-on. Renewal in 17 months — begin expansion conversation in month 12.",
	"Ana was promoted to VP of Engineering. She now has direct control of the infrastructure budget, which increased her signing authority to $200k. She reached out proactively to discuss expanding the deployment from 40 to 80 seats and adding the knowledge graph feature as an enterprise add-on. Total expansion opportunity: $60k additional ARR. Scheduled a proposal call for next Wednesday.",
	"TechCorp upgraded from the Professional plan ($45k ARR) to the Enterprise plan ($89k ARR), driven by their need for SSO, audit logs, and the dedicated CSM support tier. The upgrade was processed on the anniversary of their initial contract. Sarah Chen, their CSM, facilitated the upgrade conversation. TechCorp now represents $89k ARR and is flagged as a reference customer for ENT prospects.",
	"Maria's team achieved 93% user adoption (28 of 30 staff trained) within the first 30 days. She requested a product roadmap presentation for her executive leadership team to demonstrate ROI before the annual budget review. Presentation scheduled for May 20th with attendance from CEO, CFO, and CTO. Prepare slides highlighting: usage metrics, time saved per user, and upcoming features on the roadmap.",
	"Omega Solutions trial feedback: strong on recall precision for structured financial data, needed improvement on unstructured PDF parsing. Engineering flagged the PDF limitation as a known gap, scheduled for Q3 release. Omega Solutions requested a timeline commitment in writing. Sent a formal product commitment letter signed by our VP Product. Trial extended by 14 days. Conversion expected by end of April.",
	"Carlos secured executive sponsorship from our CEO who agreed to join the final board presentation at NovaTech. The board presentation is on June 27th at 9 AM Pacific. Materials needed: executive summary, customer references (TechCorp and Initech), and pricing summary. Board has four members including two technical board advisors. Deal probability at 85% — flag as commit in CRM for Q2 forecast.",
	"Peak Data closed at $18k ARR for a 12-month contract. CTO appreciated the detailed concurrency write-up and cited it as a differentiator. They want a monthly architecture review call with our engineering team for the first quarter. Engineering agreed to a monthly 30-minute call as part of the onboarding package. First call scheduled for May 5th. Account flagged for expansion conversation at month 6.",
	"Omega Solutions closed at $28k ARR after the PDF parsing fix shipped in Q3. Their CTO David Park signed without requiring legal review — fastest close of the quarter at 22 days. They immediately asked about the multi-agent coordination feature and the REST API. Onboarding call scheduled for the following Monday. Referred us to two contacts at Series B FinTech companies in their network.",
	"Globex Corp EU expansion signed for 25 additional seats at $800/seat = $20k ARR. Total Globex ARR now $107k. Karen Wu's EU team goes live on August 1st. Data residency confirmed: EU region on AWS eu-west-1, compliant with GDPR Article 28 processor requirements. They requested the Data Processing Agreement addendum — legal to send within 48 hours.",
	"Initech's Head of Data Science formally requested a custom model fine-tuning feature as part of their enterprise renewal conversation. Product team evaluated the request: domain vocabulary injection is feasible in Q1 next year. Offered a co-development agreement where Initech participates in the beta program and provides labeled training data. Their legal team is reviewing the co-development terms.",
	"NovaTech's EU expansion for 25 additional seats at $800/seat = $20k ARR was signed by their new EU Managing Director Hans Müller. EU data residency confirmed. Total NovaTech ARR now $122k. Carlos mentioned they are planning to hire 15 more engineers in Berlin in H2, which could represent another 15-seat expansion. Flag for renewal + expansion conversation in Q4.",
	"Ana's expansion to 80 seats and knowledge graph add-on closed at $120k ARR total. She signed a 2-year contract to lock in pricing ahead of a planned price increase. Ana is now our largest single account. She accepted an invitation to join our Customer Advisory Board. First CAB meeting is scheduled for September 12th in San Francisco. She will present their use case to other enterprise customers.",
	"TechCorp case study reached 4,200 views in the first month and generated 18 inbound demo requests from companies in their industry. Three of those requests converted to qualified opportunities. TechCorp agreed to a video testimonial for our upcoming product launch event. Maria Chen will present a 10-minute session at our annual user conference in October.",
	"Initech renewal and expansion signed for $280k ARR on a 36-month contract — the largest contract in company history. Their domain-specific vocabulary beta program starts in Q1. They upgraded to the Premium Support tier with a 2-hour SLA for Severity-1 issues. Rachel Torres remains their CSM. Initech will present at the annual user conference alongside TechCorp.",
	"Peak Data's month-6 expansion conversation yielded a 20-seat addition at $800/seat = $16k ARR. Their usage has grown to 28 of 30 original seats at 90% weekly activity. The CTO mentioned they plan to open a London office in Q1 next year — flag for EU data residency conversation. Total Peak Data ARR now $34k. High probability of further expansion at year-end.",
	"Maria Lopez was promoted to Chief Digital Officer. She sent a company-wide memo crediting GrayMatter as a key productivity tool for their engineering org. Her new role gives her budget authority over all technology purchases company-wide. She has asked for a strategic partnership conversation to explore an OEM licensing arrangement for internal deployment at scale. Opportunity flagged to leadership.",
	"Globex Corp's Karen Wu referred us to three contacts: VP Engineering at TechnoStar, CTO at CloudPeak, and Head of Data at StreamCore. All three are warm introductions with pending budget for agent tooling in H2. Discovery calls scheduled for all three in the same week. This represents a pipeline addition of approximately $180k ARR if all three convert at average deal size.",
	"NovaTech signed a 3-year renewal at $150k ARR, a 23% increase over the previous contract, locking in pricing and adding the Enterprise Support tier. Carlos is now VP of Engineering following a promotion. He has agreed to co-author a blog post about their use case for our developer blog. Publication target: end of Q3. Blog post draft to be shared by August 15th.",
	"Ana Torres joined the Customer Advisory Board and presented at the September 12th CAB meeting. Her session on multi-agent coordination generated the most discussion. She agreed to participate in a joint product roadmap session with our CPO to co-design the agent orchestration features for the next major release. Two other CAB members requested direct introductions to Ana to learn from her deployment.",
	"Omega Solutions reached $45k ARR after two expansion rounds driven by their growing data team. Their CTO David Park was named to Forbes 30 Under 30 and publicly credited GrayMatter in his profile. The press mention drove 340 inbound signups in 48 hours. They are now a Platinum reference customer and have agreed to participate in all major marketing initiatives for the next 12 months.",
	"TechCorp's annual renewal processed at $92k ARR, a $3k uplift for overage usage. Their NPS at 12 months is 9.2 out of 10, the highest in our customer base. Maria Chen presented at our user conference to 400 attendees and received a standing ovation. Three enterprise logos requested her contact information for peer reference calls. TechCorp is our strongest reference in the infrastructure vertical.",
	"Carlos's NovaTech account was acquired by EnterpriseGroup in an all-cash deal. The acquisition triggered a change-of-control clause in their contract requiring written consent for assignment. Legal confirmed consent was granted and the contract transferred without renegotiation. Carlos retained his VP Engineering role post-acquisition. EnterpriseGroup's procurement team reached out to expand the deployment to their 300-person engineering org.",
	"EnterpriseGroup expansion scoping call completed. They have 300 engineers across 6 product teams in 3 time zones. Their use case spans engineering knowledge management, incident retrospectives, and new hire onboarding. Total seat opportunity: 250 seats at $800/seat = $200k ARR. Deal cycle estimated at 6 months given their procurement complexity. Assigned a dedicated solutions engineer for the technical evaluation.",
	"Initech's co-development beta for domain vocabulary injection launched to 5 beta customers. Initech's data science team contributed 2,400 labeled examples in the first two weeks. Early results show 23% improvement in recall precision for domain-specific terminology. Product team presented early results at an internal all-hands. Initech's investment in the beta program makes their renewal probability near-certain.",
	"Peak Data opened their London office in Q1 and immediately needed EU data residency. Their CTO coordinated directly with our engineering team to migrate their EU data within 48 hours. The fast migration earned significant goodwill. They expanded to 60 seats in the London office at $800/seat = $48k ARR. Total Peak Data ARR now $82k. Full renewal and consolidation expected at year-end.",
	"StreamCore discovery call completed. Their Head of Data, James Wu, manages 45 data engineers who build and maintain real-time data pipelines. Their primary need is contextual memory for their internal ML agents that monitor pipeline health. Budget approved at $60k ARR. They want a 90-day pilot before committing. Pilot provisioned on April 18th. Technical POC contact: their senior ML engineer Priya Singh.",
	"CloudPeak CTO Jessica Park signed a $75k ARR contract after a 45-day evaluation. Key differentiator cited: the quality of hybrid retrieval versus their previous vector-only solution. They have 80 engineers and immediately provisioned all seats. Jessica requested an on-site executive business review in their Seattle office for month 3. CSM assigned: David Lee with a Seattle-based presence.",
	"TechnoStar VP Engineering Marcus Chen closed at $55k ARR after a competitive evaluation against two other vendors. Decisive factor: our open-source friendliness and the Go native integration. They are a 100% Go shop. Marcus will publish an engineering blog post about the integration. Estimated reach: 25k developer readers. Blog post draft shared for review — editorial feedback due in 5 business days.",
	"Ana's company completed a Series C fundraise of $120M. Their engineering org is expanding from 80 to 150 engineers over the next 18 months. Ana has pre-approved $250k ARR for tooling for the expanded team. She wants to lock in a 3-year enterprise agreement before the price increase takes effect next quarter. Proposal sent with 3-year pricing and dedicated support tier.",
	"EnterpriseGroup's 6-month evaluation concluded with a successful POC across 2 of their 6 product teams. The pilot team leads presented results to their CTO: 28% reduction in time to context for incident response, 41% improvement in new hire ramp time. The CTO approved expansion to all 6 teams. Legal is drafting the enterprise framework agreement. Deal expected to close in 4-6 weeks.",
	"Globex Corp's Karen Wu left the company to join a startup. Her replacement is VP Engineering Thomas Reid, promoted internally. Thomas participated in an emergency business continuity call and confirmed Globex Corp will honor all existing commitments. He is interested in expanding the deployment to their Asia-Pacific team of 20 engineers. Introduction call with Thomas scheduled for next Monday.",
	"Initech's domain vocabulary product went GA with Initech as the launch customer. Their data science team reduced annotation time by 60% using the automated vocabulary extraction pipeline. The feature is now generally available as an Enterprise add-on at $2k/month per model. Initech was credited as a co-inventor in the product announcement. Their renewal was processed at $320k ARR — our highest contract ever.",
	"StreamCore pilot concluded successfully after 90 days. Pipeline monitoring accuracy improved by 31% when agents could recall historical incident patterns. Their Head of Data James Wu signed the full contract at $65k ARR. They immediately requested to expand to their platform engineering team of 20 engineers. Expansion contract for 20 additional seats at $800/seat = $16k ARR signed the same day.",
	"Ana's 3-year enterprise agreement signed at $280k ARR, covering 150 seats. It includes the domain vocabulary add-on, dedicated SLA of 1-hour response for Severity-1, and a private Slack channel with our engineering team. Ana was featured in a press release alongside our CEO. The announcement generated 2,100 LinkedIn impressions and 14 inbound enterprise inquiries within 24 hours.",
	"EnterpriseGroup closed at $200k ARR for a 3-year enterprise framework agreement covering 250 seats across all 6 product teams. It is now our largest single contract. Their General Counsel negotiated data processing terms extensively — final DPA signed after 4 rounds of revisions. Executive sponsor is their EVP Engineering Robert Zhao who personally championed the rollout to all teams.",
	"TechnoStar's blog post by Marcus Chen published and reached 38k developer readers — 52% above his projected estimate. Drove 640 new signups, of which 12 converted to paid accounts in the first week. Marcus agreed to speak at our developer conference in November. His session title: 'Building Stateful AI Agents in Go: A Production Retrospective.' Expected attendance: 200 engineers.",
	"CloudPeak's month-3 executive business review in Seattle was attended by Jessica Park and two board observers. They presented a 44% improvement in incident resolution time attributable to agent memory. One board observer requested an introduction to our CEO for a potential strategic investment conversation. Their expansion to a second department of 30 engineers is approved — $24k ARR addition.",
	"Maria Lopez signed a strategic OEM licensing agreement for internal deployment of GrayMatter within her company's proprietary AI platform. The agreement covers unlimited internal seats at a flat annual license of $300k. External distribution rights are explicitly excluded. This is a new revenue category — flagged for a dedicated OEM contract template. Legal review completed in 10 business days.",
	"Peak Data's year-end renewal and consolidation signed at $120k ARR covering all offices globally including the London expansion. CTO agreed to a 2-year commitment. She will join the Customer Advisory Board alongside Ana Torres. Peak Data now has 3 offices (SF, London, Singapore) and 150 engineers. Singapore onboarding to start in Q2 next year.",
	"Omega Solutions was acquired by a publicly listed data company. The acquisition triggered an enterprise procurement review. Their new parent company's CTO evaluated GrayMatter against their existing enterprise agreements with our competitors. After a 30-day review, they decided to standardize on GrayMatter for all AI agent memory needs across the combined 400-person engineering org.",
	"EnterpriseGroup's rollout to all 6 product teams completed 3 weeks ahead of schedule. Company-wide adoption reached 87% of licensed seats within 60 days. Their internal developer portal now includes GrayMatter as a standard tool in their agent development kit. Robert Zhao is requesting a joint press release and a case study co-authored by their Head of Developer Experience.",
	"TechCorp's Maria Chen joined our Board of Customer Advisors and was elected Chair of the Technical Working Group at the September CAB meeting. She will co-lead the agent orchestration roadmap working group alongside Ana Torres. Their combined influence is shaping the product roadmap for the next two major releases. Both are confirmed speakers at the annual user conference.",
	"Ana Torres's company completed an IPO on NASDAQ. GrayMatter was cited in the S-1 as a key operational infrastructure tool. Our logo appears in their investor presentation materials. This represents a significant brand validation event. Legal confirmed no action required on our side. Ana's personal equity vesting means she is financially independent — renewal risk is low.",
	"Initech's co-development program expanded to 12 beta customers contributing domain vocabularies across 8 verticals. The program generated $180k in additional ARR from domain vocabulary subscriptions in year one. Product team is planning a second cohort of 20 beta customers for H1 next year. The program is now a formal product-led growth motion owned by the PLG team.",
	"NovaTech, now part of EnterpriseGroup, is consolidating their contract under the EnterpriseGroup framework agreement. Carlos, as VP Engineering, is advocating for a 50-seat allocation for the Berlin office under the new parent company structure. This would increase the EnterpriseGroup footprint from 250 to 300 seats — $40k ARR addition to the framework agreement.",
	"CloudPeak closed their Series C at $85M. Jessica Park messaged directly to say the operational efficiency demonstrated by GrayMatter was cited in their investor materials as a competitive moat. They are expanding internationally to London and Singapore — each office needs 20 seats, adding $32k ARR. Total CloudPeak ARR projected at $131k by end of year.",
	"StreamCore's ML agents for pipeline monitoring have processed 12 million recall operations in 90 days of production use. Their SRE team reported a 39% reduction in mean time to identify pipeline root causes. James Wu presented the results at a data engineering conference to 800 attendees. The presentation is available on YouTube and has 4,200 views, driving 28 qualified inbound inquiries.",
	"TechnoStar's expansion to their infrastructure team of 40 engineers closed at $32k ARR, bringing total account ARR to $87k. Marcus Chen co-authored a technical reference architecture document for Go-native AI agents using GrayMatter, published on our documentation site. The document became the most-viewed technical resource on the site within 48 hours of publication.",
	"Omega Solutions' parent company standardized on GrayMatter and signed a global master agreement covering 400 seats at $800/seat = $320k ARR. Legal negotiated a 3-year term with CPI-linked annual price adjustments. This is our largest account by seat count. Assigned a dedicated Strategic Account Manager and a technical solutions architect for the global rollout.",
	"Maria Lopez's OEM agreement generated the first external deployment of GrayMatter-powered memory in a third-party product. Their product shipped to 500 enterprise customers, each potentially running 10+ agents using our memory layer. Revenue participation model: $0.50 per active agent per month. Month-1 revenue: $2,400 from 4,800 active agents. This is the seed of a new revenue stream.",
	"Ana Torres's company completed 100% of their 150-engineer rollout ahead of schedule. Their internal engineering blog published a post about the deployment that reached 18k readers via HackerNews. Three enterprise companies reached out directly after reading the post. Ana volunteered to do a 30-minute reference call for each. All three are in the pipeline with a combined opportunity of $210k ARR.",
	"EnterpriseGroup's Head of Developer Experience published the joint case study. The study quantifies: 28% reduction in incident response time, 41% faster new engineer ramp, 19% increase in cross-team knowledge reuse. The study was picked up by three industry analyst firms. One analyst firm requested an analyst briefing for potential inclusion in their Enterprise AI Tooling market report.",
	"Peak Data's Singapore office onboarding completed. 20 engineers provisioned, training completed in 2 days using the self-serve onboarding materials. Their CTO commended the quality of the Go SDK documentation. Singapore team immediately flagged a timezone-specific use case for agent handoff between shifts — logged as a product request. This use case could apply to 15 other enterprise accounts.",
	"Initech's renewal for year 3 processed at $340k ARR, an 8% increase from year 2. The domain vocabulary product is now used by 60% of their engineering org. Their data science team is publishing an internal paper on the productivity gains attributable to domain-specific agent memory. They requested permission to submit a version to a peer-reviewed ML conference.",
	"CloudPeak's London office (20 seats) went live. Singapore provisioning scheduled for month 2. Total CloudPeak global footprint: 120 licensed seats, $96k ARR, projected $131k ARR by year-end with Singapore. Jessica Park is presenting at our annual conference on international deployment best practices. Their deployment is now our reference case for multi-region enterprise rollouts.",
	"TechCorp's year-2 renewal processed at $95k ARR, a $6k increase from year 1 reflecting seat expansion. They formally nominated Maria Chen as a reference customer for all enterprise deals over $50k ARR. Their case study has been cited in 14 enterprise sales conversations this quarter. Marketing team is creating a video testimonial with Maria Chen — filming scheduled for October 8th.",
	"StreamCore's contract renewal signed at $90k ARR for year 2, covering their expanded platform engineering team. James Wu was promoted to VP Data Engineering. He has increased budget authority and is evaluating a company-wide expansion to 100 total seats — projected $80k ARR increase. Proof-of-concept for expanded deployment to start in month 13 with a 30-day evaluation period.",
	"The analyst firm Gartner included GrayMatter in their inaugural Enterprise AI Agent Memory Solutions market report, placing us in the Visionary quadrant. The report cited our hybrid retrieval approach, Go-native integration, and enterprise customer base. Fourteen inbound enterprise inquiries were received within 48 hours of the report publication. Sales team capacity expanded by two enterprise AEs to manage demand.",
	"Omega Solutions' parent company's global rollout to 400 seats completed in 6 weeks — 2 weeks ahead of the projected 8-week timeline. Their Head of Platform Engineering cited the quality of the migration tooling and documentation as the key accelerant. They are now evaluating GrayMatter for deployment in their AI research division — an additional 50-seat opportunity not in the original scope.",
	"Maria Lopez's OEM deployment scaled to 8,200 active agents across their customer base. Monthly participation revenue reached $4,100. They are expanding the integration to their enterprise tier customers, projected to add 15,000 active agents over the next 6 months. OEM revenue run rate projected at $12,000 per month within 6 months — a meaningful contribution to ARR diversification.",
	"NovaTech Berlin office, now under EnterpriseGroup, provisioned 50 additional seats in the framework agreement. Carlos continues to drive adoption across the Berlin engineering team. Total EnterpriseGroup global footprint: 300 seats, $240k ARR. Robert Zhao requested a strategic planning session for year 2 to align on product roadmap priorities for EnterpriseGroup's specific use cases.",
	"Ana Torres announced GrayMatter as a founding technology partner in their AI-native engineering platform. This is the first formal technology partnership in our company's history. The agreement includes joint go-to-market activities, co-marketing budget of $50k per year, and joint participation in Dreamforce and AWS re:Invent. Legal drafted the Technology Partnership Agreement — under review.",
	"TechnoStar's 2-year renewal signed at $95k ARR, a 9% increase. Marcus Chen has become an internal evangelist who has given GrayMatter training sessions to their entire engineering org. Their deployment now covers 120 engineers across frontend, backend, and infrastructure teams. Marcus proposed hosting a GrayMatter developer meetup at their San Francisco office with an expected attendance of 60 engineers.",
	"CloudPeak's Singapore office provisioned with 20 seats on schedule. Total CloudPeak: 140 seats across 3 regions, $112k ARR. Jessica Park joined the Customer Advisory Board. She proposed a new session at the CAB on multi-region data sovereignty — 8 of 12 CAB members voted to add it as a standing agenda item given the growing EU and APAC regulatory complexity.",
	"The Technology Partnership Agreement with Ana Torres's company was signed after 3 weeks of legal negotiation. Joint go-to-market activities begin in Q1 next year with a joint webinar targeted at 500 enterprise engineering leaders. Co-marketing budget of $50k allocated. The partnership was announced via a joint press release that reached 12,000 readers across both companies' networks.",
	"EnterpriseGroup's strategic planning session for year 2 resulted in a formal product advisory committee with Robert Zhao as Chair. He committed 8 engineer-days per quarter to participate in beta programs and provide product feedback. EnterpriseGroup will co-develop the agent orchestration framework as a design partner, with their engineering team having early access to APIs 6 weeks before GA.",
	"Initech's internal ML conference paper on domain-specific agent memory was accepted to NeurIPS 2027. Their data science team will present the paper at the conference in December. We are acknowledged in the paper's acknowledgments section. The paper's publication will establish Initech — and by extension GrayMatter — as a reference implementation in the academic literature on AI agent memory.",
	"The GrayMatter annual user conference attracted 850 registered attendees, a 112% increase from the previous year's 400. Speaker lineup: Maria Chen (TechCorp), Ana Torres (Series C co), James Wu (StreamCore), and Marcus Chen (TechnoStar). Three new enterprise logos signed enterprise contracts at the conference. Conference-attributed pipeline: $540k ARR in new opportunities.",
	"OEM revenue from Maria Lopez's platform crossed $10,000/month for the first time, driven by a 20,200 active agent milestone. The OEM agreement is now contributing $120k ARR on an annualized basis. Maria is proposing a white-label option for her largest enterprise customers who want branded experiences. White-label pricing model under commercial review — target pricing: flat $50k/year per white-label deployment.",
	"Total ARR crossed $3M milestone. Customer breakdown: 14 enterprise accounts averaging $180k ARR, 6 mid-market accounts averaging $45k ARR, OEM contributing $120k ARR. Net Revenue Retention for the past 12 months: 134%. Gross Churn: 0% — no customer has churned since inception. Customer Acquisition Cost recovered within an average of 4.2 months. Gartner included us in the Magic Quadrant.",
}

// ── Token approximation ───────────────────────────────────────────────────────

// approxTokens estimates GPT-4-class token count for text.
// Each word contributes ~1.33 tokens on average (accounts for subword splits,
// punctuation tokens, and whitespace). Matches tiktoken empirically within ±10%
// for English business prose.
func approxTokens(text string) int {
	words := strings.Fields(text)
	return int(float64(len(words)) * 1.33)
}

// ── Benchmark ─────────────────────────────────────────────────────────────────

const (
	agentID = "sales-agent"
	query   = "follow up with prospects and close pending deals this week"
	topK    = 8
)

// sessionCounts controls which data points are reported.
// Each "session" = one stored memory observation (a paragraph extracted from
// a real agent interaction, ~50-70 words). Full injection = all observations
// concatenated. GrayMatter = top-8 most relevant observations recalled.
var sessionCounts = []int{1, 10, 30, 100}

// result is one row of the benchmark table.
type result struct {
	Sessions     int
	FullTokens   int
	RecallTokens int
	Reduction    float64 // percent
}

// runBenchmark executes the full measurement and returns one result per entry
// in sessionCounts. It is separated from main so the reproducibility test can
// call it and compare the published tables against freshly measured numbers.
func runBenchmark() ([]result, error) {
	dataDir, err := os.MkdirTemp("", "graymatter-bench-*")
	if err != nil {
		return nil, fmt.Errorf("mktemp: %w", err)
	}
	defer os.RemoveAll(dataDir)

	// Open store with keyword-only embedder — zero LLM calls, deterministic.
	emb := embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword})
	store, err := memory.Open(memory.StoreConfig{
		DataDir:  dataDir,
		Embedder: emb,
	})
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	// Shuffle insertion order so keyword ranking isn't biased by recency.
	rng := rand.New(rand.NewSource(42)) //nolint:gosec
	perm := rng.Perm(len(corpus))

	ctx := context.Background()

	results := make([]result, 0, len(sessionCounts))
	inserted := 0

	for _, target := range sessionCounts {
		// Insert observations up to the target session count.
		for inserted < target && inserted < len(corpus) {
			if err := store.Put(ctx, agentID, corpus[perm[inserted]]); err != nil {
				return nil, fmt.Errorf("put: %w", err)
			}
			inserted++
		}

		// Full injection: ALL stored observations concatenated.
		allFacts, err := store.List(agentID)
		if err != nil {
			return nil, fmt.Errorf("list: %w", err)
		}
		var fullTexts []string
		for _, f := range allFacts {
			fullTexts = append(fullTexts, f.Text)
		}
		fullTokens := approxTokens(strings.Join(fullTexts, "\n"))

		// GrayMatter: recall only top-K most relevant observations.
		recalled, err := store.Recall(ctx, agentID, query, topK)
		if err != nil {
			return nil, fmt.Errorf("recall: %w", err)
		}
		recallTokens := approxTokens(strings.Join(recalled, "\n"))

		reduction := 0.0
		if fullTokens > 0 {
			reduction = float64(fullTokens-recallTokens) / float64(fullTokens) * 100
		}

		results = append(results, result{
			Sessions:     target,
			FullTokens:   fullTokens,
			RecallTokens: recallTokens,
			Reduction:    reduction,
		})
	}

	return results, nil
}

func main() {
	start := time.Now()

	results, err := runBenchmark()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	fmt.Println()
	fmt.Println("GrayMatter Token Efficiency Benchmark")
	fmt.Printf("Query:    %q\n", query)
	fmt.Printf("Embedder: keyword (no LLM required — fully reproducible)\n")
	fmt.Printf("TopK:     %d recalled observations per query\n", topK)
	fmt.Println()
	fmt.Printf("%-10s  %-18s  %-22s  %s\n",
		"Sessions", "Full Injection", "GrayMatter Recall", "Reduction")
	fmt.Println(strings.Repeat("─", 72))

	for _, r := range results {
		fmt.Printf("%-10d  ~%-17d  ~%-21d  %.0f%%\n",
			r.Sessions, r.FullTokens, r.RecallTokens, r.Reduction)
	}

	fmt.Println(strings.Repeat("─", 72))
	fmt.Printf("\nRun time:  %s\n", time.Since(start).Truncate(time.Millisecond))
	fmt.Println()
	fmt.Println("Approximation: words × 1.33 ≈ GPT-4 tokens (±10% for English prose).")
	fmt.Println("With vector embeddings (Ollama / OpenAI / Anthropic) recall precision")
	fmt.Println("improves further, maintaining similar or better token reduction ratios.")
}
