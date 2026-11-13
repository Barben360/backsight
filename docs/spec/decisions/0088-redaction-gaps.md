# 0088 — Redaction gaps: redact before cutting, more prefixed formats, cut private keys

- **Date**: 2026-11-12
- **Status**: accepted
- **Decided by**: author (2026-11-12: "Run all fixes that you can do deterministically and you think useful right now, especially for the poisoning fixes and the redaction fixes … You are autonomous on that"), on the deep review of 2026-11-12

## Context
The deep review of 2026-11-12 ran the redaction rules (SPEC §13, `internal/redact`) against probes and found three kinds of gap:

- **Cut, then redacted.** The prompt hook cut the recall query to `recall.query_chars` runes, then redacted it: a key straddling the cut fell below its pattern's minimum length and its prefix left as a query. The topic-shift vocabulary (first 4,000 runes), the end of the last reply (last 1,000 runes) and a failed command in the failure query (first 100 runes) were cut the same way. The transcript and Codex rollout action lines already redacted first.
- **False negatives.** SendGrid (`SG.<22>.<43>`), Tailscale (`tskey-…`), a bare 40-character AWS secret access key (its `/` and `+` split it below `high_entropy`'s 32), Telegram bot tokens (only the part after the colon was masked, by `high_entropy`), Hugging Face (`hf_` and 34 letters: no digit, so no `high_entropy`), Groq, PyPI, Vault and Mailgun (`key-` and 32 hex) got through whole or in part.
- **False positives that destroy text.** A `-----BEGIN … PRIVATE KEY-----` header with no `END` line (in a `grep` command, a sentence, a Go string) swallowed everything to the end of the text. (Identifiers made of words with a digit or two, such as `CamelCaseIdentifier2Version3FooBar9Baz`, are also masked as high-entropy tokens: that loss is kept, D5.)

## Decision
### D1 — Redact before cutting
Every text that is cut to its start or end before it is sent or kept is redacted first: the recall query, the topic-shift vocabulary, the failed command of the failure query (whitespace collapsed first, as `clip` does) and the end of the last reply. Redacting a whole long paste in a hook costs too much (the rules run at about 0.5 MB/s), so a window is redacted: the kept runes plus 4,096 (`redactMargin`). A secret straddling the cut is seen whole; one longer than the margin is still past its pattern's minimum length where the window cuts it. When redaction shrinks the window to within half a margin of the cut (a long key replaced), the window doubles and is redacted again, up to 65,536 runes (`redactMaxWindow`: a single multi-megabyte token would otherwise be redacted whole, seconds per call, twice per prompt). Still short at the cap, it fails closed: the redacted window's last 4,096 runes, where a key cut at the cap would sit, are dropped (its first ones for the reply's end), even when that leaves less than was asked or nothing. A `[redacted:<kind>]` placeholder may itself be cut: it reveals nothing.

The reply's end is cut at its front, so its window could start inside a private key block, leaving the key's body without its header (then only `high_entropy` sees it, and base64 split at `/` or `+` escapes it). Such a window starts at the block's header instead, when that is within the cap; beyond it, the window starts after the block's `END` line, or is empty when there is none.

### D2 — More prefixed formats
New kinds, each from a prefix distinctive enough that a match is a credential (gitleaks' default rules, https://github.com/gitleaks/gitleaks/blob/master/config/gitleaks.toml, and the AI providers' documented formats): `sendgrid_key`, `tailscale_key`, `huggingface_token` (`hf_`, `api_org_`), `groq_key`, `xai_key`, `replicate_token`, `perplexity_key`, `pypi_token`, `vault_token` (`hvs.`, `hvb.`, `hvr.`), `mailgun_key`, `doppler_token`, `dynatrace_token`, `linear_key`, `notion_token` (`ntn_`), `databricks_token`, `pulumi_token`, `postman_key`, `rubygems_key`, `sentry_token`, `planetscale_token`, `prefect_key`, `grafana_token`, `heroku_key`, `brevo_key`, `square_token`, `artifactory_key`, `flyio_token`, `openshift_token`, `onepassword_token`, `newrelic_key`, `alibaba_key`, `atlassian_token`, `azure_client_secret`, `bedrock_key`, `supabase_key`, `sourcegraph_token`, `cloudflare_key` (origin CA), `typeform_token`, `easypost_key`, `shippo_token`, `clojars_token`, `facebook_token`, `telegram_token`, `teams_webhook`, `nuget_password`. Existing kinds widened: Stripe `sk_prod_` and `whsec_`; Slack `xoxe-`, `xoxo-` and `xapp-`; AWS key ids `ABIA`, `ACCA`, `A3T…`; GitLab `glft-`, `glsoat-`, `glffct-`, `glagent-` and `GR1348941…`.

Each new prefixed rule names the literal its match must hold, and a text holding none of them skips the rule: the redaction of a 100 KB text without them costs what it did before.

A Telegram token is masked whole, its bot id included (after `bot` in an API URL too); a value after the colon made of words (`12345:AssertionError…`, D3's test of words) stays. A Mailgun key is not one after a word character or a `-` (`cache-key-<md5>`, a CI cache key).

The rules whose match is bounded by the characters around it (`aws_secret_key`, `telegram_token`, `azure_client_secret`, `mailgun_key`) check those neighbours without consuming them: two credentials one separator apart (`<key> <key>`, `<key>,<key>`) are both masked.

### D3 — The bare AWS secret access key
`aws_secret_key`: exactly 40 characters of `[A-Za-z0-9/+]`, holding a `/` or a `+`, with no `[A-Za-z0-9/+=_.-]` on either side (a `.` after it only at the end of a sentence), with upper and lower case, a digit and a change of character class on 30% of its characters, its `/`-separated parts not all made of words: lower-case and capitalised words each holding a vowel, and runs of one or two digits (a path such as `src/Header/MenuItem2`; a random value almost never has that shape). File paths, URLs, Go import paths and file names hold a `.`, `_` or `-` beside the 40 characters or are made of words; a git SHA is hex, with no upper case or no lower case; 40 characters with no `/` or `+` are left to `high_entropy`.

### D4 — A private key cut before its END line
`private_key` reads each header up to the next header only (so the work stays linear, however many headers a text holds):
- **terminated**: an `END` line before the next header: the header to that line;
- **cut**: else, fail-closed, every line from the header to the first line of prose (three words or more, each under 16 characters) or the end of the text, once a line held key material. A line is key material when, past its prefix, it holds a base64 run that is not made of words (D3's test) and is 40 characters or more mixing upper case, lower case and digits (key lines are 64 or 76), or 16 or more that looks random (upper and lower case, a digit, a change of character class on 30% of its characters); or when it is eight groups or more of 4 to 8 base64 characters mixing all three (a key printed in spaced groups, which would otherwise read as prose). Code, identifiers (`ErrPrivateKeyDetectedInInput`, CamelCase), paths made of words and short lower-case runs do not start the material, so a header quoted before them keeps them. Lines break at a newline, real or escaped once or twice (`\n`, `\\n`, `\r\n`). A quote, comment or diff prefix (`>`, `#`, `//`, `*`, `|`), opening quotes and a trailing backslash or `$` (`cat -A`) are no part of a line. Before the material, armour headers (`Proc-Type:`, `DEK-Info:`, `Version:`, `Comment:`) are read as part of the block; blank lines are bridged; a string closed right after base64 (`…=="` in JSON or code) ends the key there. Once the material started, lines of one or two short words after it (`Thanks`, `bye now`) are taken with it: losing them costs less than leaking a line of key.
- **a header with no key material before the first prose line** (quoted in a `grep`, named in a sentence, in a Go string) is no secret: it stays, with the text after it.

### D5 — Identifiers made of words stay masked
`high_entropy` is unchanged: a CamelCase identifier with digits (`CamelCaseIdentifier2Version3FooBar9Baz`) is still masked. A keep rule for identifiers made of words was tried and dropped (orchestrator, 2026-11-12, on the review of this decision's pull request): it let through passphrases made of words that the rule masked before (`MyPasswordIsSecretHorse1CorrectBattery2Staple3`, `Tom2Jerry3Cat4Dog5Fish6Bird7Bear8Lion9Wolf0Fox`), and an identifier lost from a memory costs less than a leaked passphrase.

### D6 — Migration
Redaction applies to what is sent from now on. Nothing about stored memories changes: `backsight memory scan` uses the same rules, so it now also finds the new formats in what was stored before, and gives the purge command for each.

## Not done, and why
- **Rules keyed on a service's name** (gitleaks' `cisco-meraki`, `okta`, `twitter`… rules: a service word, then a value of a given shape): they need the key word and are covered by the secret-named key rules when the key holds `key`, `token` or `secret`; the others are too broad for text that is mostly prose.
- **Short or weak prefixes**: Vault's legacy `s.<24>`, Octopus `API-<26>`, ClickHouse `4b1d…`, Notion's legacy `secret_<43>` (an identifier `secret_…` of 43 letters is plausible in code), Mailgun `pubkey-` (a public key).
- **Kubernetes `kind: Secret` YAML data values**: base64 values of any key, which needs a YAML context the rules do not keep.
- **Whole-text redaction in hooks**: too slow on a long paste (D1's window instead).
