# 0009 — Full configuration in the web admin

**Status** — Proposed (TASK-0017, 2026-11-26; revised in iterations 2
and 3).

## Context

M4 lets the operator configure everything that `config.toml` holds from
the admin UI, without opening the file ([roadmap M4][rm-m4]). The user
decided on 2026-11-26:

- All UI text is English.
- The UI is a finished product, designed for appearance and convenience,
  not one form field per key: one visual system across the dashboard and
  the settings, light and dark themes, phone-width layouts, field-level
  validation, and guidance in place of the starter file's comments. A
  clickable prototype is approved before implementation.
- A form save rewrites the whole file in cuttle's canonical TOML form with
  short generated comments; comments written by hand are not kept. The
  previous file stays in `.bak`, and the page shows the change as a diff
  before it is applied. The TOML text editor stays as an advanced page.
- Every form save goes through the existing validation, `base` conflict
  check, atomic write, and reload ([0005 D10][d5-10]). The validator stays
  the single authority on what is allowed.
- The admin password can be changed in the UI with the current password.
  The first password is still set with `cuttle admin passwd`, so another
  local user cannot claim it first.

After the first review of this record, the user decided, also on
2026-11-26:

- The prototype at commit `6421b01` is approved as the visual reference.
- The optional `note` key is added to rules, upstreams, and interfaces
  (D6).
- The password change shares the login backoff and its failure count
  (D12).

What exists: the M2 dashboard (`/`, `/domains`, `/live`) and the text
settings page (`/settings`) of [0005 D9 and D10][d5-9], server-rendered
with `html/template`, styled by one embedded stylesheet, under a
Content-Security-Policy that refuses inline script and style. The
configuration file stays the only source of truth ([0004 D2][d4-2]).
[0004 D1][d4-1] said that cuttle never encodes TOML, and 0004 and 0005
rejected forms that rewrite the file because they would lose comments.
The user's decision above reverses that; this record defines the writer
that replaces it and lists the amendments under
[Changes to other records](#changes-to-other-records).

The clickable prototype is [0009-prototype.html](0009-prototype.html). It
is the visual reference: where this text and the prototype disagree on
appearance, the prototype wins; on behavior, this text wins. The user
approved it at commit `6421b01`; iteration 2 changed it only where this
revision changed behavior, the default match of Route a host (D3.12) and
the generated header comment (D6). Open it in a
browser from the repository; it needs no server and loads nothing
remote. It shows the shell and navigation, the overview, the rules chain
with Test a URL, filtering, and drag, keyboard, and menu reordering, the
rule editor with list inputs, inline errors and warnings, and the draft
test, the upstreams list and editor, the interfaces page with the
detected table and Define…, the Proxy and Account forms, a domains table
with Route…, the review dialog with its summary, notices, and diff, the
toasts, and the empty, read-only, restart-required, and
statistics-unavailable states. A striped bar at the top, which is not
part of the product, switches the state and the appearance. Its script
mimics the server (validation messages, the canonical writer, and the
explanation) so that the flows can be clicked through; the canonical
text it writes for its example parses to the same configuration as its
hand-written example file.

## Decisions

### D1. Principles

- **One product.** The dashboard and the configuration share one shell,
  one navigation, one stylesheet, and one vocabulary. Every route name,
  rule name, upstream, and interface on a traffic page links to its
  configuration, and every configuration entity shows its recent traffic
  where the statistics allow (D9).
- **The file stays the source of truth.** Forms are one more writer of
  the file. There is no draft store, no staged change set, and no second
  copy of the configuration on the server. Each page saves its own edit
  in one step: edit, review, apply.
- **The validator decides.** The form layer parses only what TOML needs
  to type a value (integers) and the two explicit choices of D4, and
  reports everything else through the
  [0004 D4][d4-4] validator run on the candidate file. The UI never
  re-implements a validation rule; it maps the validator's key paths to
  fields (D7).
- **Server-rendered, progressively enhanced.** Pages are `html/template`
  output. One same-origin script adds live feedback, drag and keyboard
  reordering, dialogs, and list inputs, and every flow also works without
  it (D10).
- **Proportionate.** No new Go module. New code goes into packages that
  already own the concern, plus a small line-diff package (D13).

### D2. Information architecture and navigation

The shell has a sidebar of three groups. Group labels are in sentence
case and name what the pages are for.

| Group | Page | Path | Content |
| --- | --- | --- | --- |
| Traffic | Overview | `/` | Where traffic went in the window, top hosts per route, upstream and interface states, configuration summary |
| Traffic | Domains | `/domains` | Host table with filters, now also by rule |
| Traffic | Live | `/live` | Active tunnels and live counters |
| Configuration | Rules | `/rules` | Ordered rules, Test a URL, the defaults at the end of the chain |
| Configuration | Upstreams | `/upstreams` | Upstream proxies with state, usage, and traffic |
| Configuration | Interfaces | `/interfaces` | Interface definitions and the interfaces detected on this Mac |
| Configuration | Proxy | `/proxy` | Listeners, the default route for local clients, timeouts, limits, the default interface list |
| Configuration | Remote clients | `/remote` | `[proxy.remote]` |
| Configuration | General | `/general` | Instance name, admin listener, statistics, log level |
| Admin | Account | `/account` | Password change, sessions |
| Admin | Advanced | `/settings` | TOML text editor and the previous version (`.bak`) |

Order follows frequency of use: rules first, then what rules refer to
(upstreams, interfaces), then settings that change rarely. Advanced keeps
its `/settings` path so that bookmarks and the M2 tests keep working.

Shell parts, on every session page:

- **Sidebar.** The cuttle mark and the instance name at the top, the
  three groups, and at the bottom the instance state: version, uptime,
  when the running configuration was loaded, and the sign-out button.
  The current page has `aria-current="page"`.
- **Banners**, above the page header, in this order, at most one of each:
  the file on disk has errors (D3.13); the file differs from the running
  configuration; the last reload failed; restart required. The statistics
  notice appears only on pages that show statistics. Banners state the
  consequence and the action, for example "proxy.listen changes apply at
  the next start. Restart with `brew services restart cuttle`."
- **Page header.** Title, one sentence on what the page controls, and the
  primary action on the right ("Add rule", "Add upstream").
- **Toasts** report the result of an apply (D7) at the top right, or at
  the bottom on a phone.
- **Phone width** (below 760 px). The sidebar becomes a top bar with the
  mark, the page title, and a "Menu" button that opens the navigation.
  The button is a `<details>` element, so it works without the script.

Every page has a skip link to the main content and exactly one `h1`.

### D3. Screens

Each screen lists its content, its states, and its interactions. The
common states are in D3.13. "Window" on configuration pages is a fixed
24 hours (D9).

#### D3.1 Overview

Changes to the [0005 D9][d5-9] overview, delivered in M4.4:

- The page opens with one sentence that answers the operator's question,
  built from the route totals of the window, for example "In the last
  24 hours, 61 % of traffic went direct, 38 % through gateway, and 2,310
  requests were blocked." Under it, one stacked bar splits the bytes by
  route in the route colors (D11), with a legend that names each route,
  its bytes, and its share.
- The per-route top-host tables stay, each host with a "Route…" action
  (D3.12).
- Upstream and interface states become compact rows with state badges;
  each name links to its configuration page.
- A configuration summary: the number of rules, upstreams, and
  interfaces, when the file was last saved, and the restart-required
  keys, with links.
- The live counters and the status table stay below.

States: statistics unavailable replaces the sentence, bar, and host
tables with the reason and "The proxy serves without statistics"; an
empty window says "No traffic in this window" and offers the 7-day
window.

#### D3.2 Domains and Live

- Each host row gets a "Route…" action (D3.12). A folded row (`*`) has
  none.
- `/domains` gains a `rule` filter, a rule name or `all`, validated like
  the other filters ([0005 D9][d5-9]). Rule traffic on configuration
  pages links here.
- `/live` keeps its 5-second refresh. Its rows get the same action; the
  action is a link, so the refresh never interrupts a form.

#### D3.3 Rules

The page shows the rules as what they are: a chain tried from top to
bottom, where the first match decides.

- **Test a URL** sits at the top: one input that accepts a URL, a
  `host:port`, or a bare host, and a Local/Remote switch. The result
  card names the matching rule and its position, the route with the
  upstream's address and disclosure mode, the interface list in force,
  and the reason when no rule matched (D8). In the list, the matching
  rule is marked, the rules above it are shown as tried and passed, and
  the list scrolls to it.
- **Filter** narrows the list by name, note, pattern, or action. While a
  filter is active, reordering is off and the page says so.
- **The chain.** Each rule is one row: its position, a drag handle, the
  name and note, its patterns as chips (hosts, domains, and CIDRs, with
  "+12 more" after the first few), its ports, an action badge, flags
  (loopback, private, an interface list), and the requests and bytes it
  decided in the last 24 hours. A thin vertical line joins the rows, with
  a node per rule in the color of its action, and ends at a last row,
  "No rule matched", that shows `proxy.default` for local clients and
  `proxy.remote.default` for remote clients with a link to their pages.
  Validator warnings on a rule, such as a pattern that an earlier rule
  already catches, show as a warning badge on the row.
- **Reordering.** Drag a row by its handle, or focus the handle and use
  the keyboard (D11), or use the row menu (Move to top, Move up, Move
  down, Move to bottom). Moves change the order on the page only; a bar
  appears, "Order changed. Discard / Review changes", and the review
  applies all moves in one save. Without the script, each row shows a
  position field and the bar's button submits the new positions.
- **Row menu:** Edit, Duplicate, Move…, Delete. Delete opens the review
  directly; the review shows the rule's 24-hour traffic and the route
  that its traffic takes after the delete, which is the decision of the
  remaining rules for the rule's first pattern.

States: with no rules, the chain shows only the default row and an empty
state, "No rules yet. Every request takes the default route: direct for
local clients, block for remote clients", with "Add rule" and "Test a
URL" still available.

#### D3.4 Rule editor

`/rules/new` and `/rules/{name}`. A two-column layout at desktop width:
the form on the left, the test panel on the right; one column below
1100 px.

Form, in this order:

1. **Name** (required; suggested from the first pattern for a new rule)
   and **Note** (optional, one line, D6).
2. **Match.** Hosts, Domains, CIDRs, and Ports, each a list input (D5).
   The hint under each says what it matches, for example "A name and all
   of its subdomains: example.com matches a.example.com". At least one
   entry overall is required; the validator says so.
3. **Action.** A segmented control: Direct, Upstream, Block. Upstream
   shows a select of the configured upstreams with each one's address
   and state, and a link to add one.
4. **Direct options**, shown only for Direct: "Allow loopback and this
   Mac's own addresses" (`allow_loopback`), "Allow private addresses that
   names resolve to" (`allow_private`), each with one sentence from
   [0003 D11][d3-11], and the interface list (D3.8 list editor) with the
   choice "Use the default list (proxy.interfaces)" or "Only these
   interfaces, in this order".
5. **Position**, for a new rule only: "First", "Before …" with a select
   of rules, or "Last". The default is first, because a new rule is
   usually an exception to the rules below it.

Test panel: the Test a URL input, tested against the configuration with
this draft applied. It answers two questions: whether this rule matches
the target, and what the whole chain decides, so it shows "Rule 1
`loopback` matches first" when an earlier rule shadows the draft. For an
existing rule it also lists the rule's top 10 hosts in the last 24 hours,
which shows what the rule catches today.

The form's footer holds "Review changes", "Cancel", and, for an existing
rule, "Delete". Command-Enter or Control-Enter reviews.

States: a draft with errors shows each error under its field and an
error summary at the top that links to the fields (D7); a valid draft
shows warnings the same way in amber. Leaving with unsaved changes asks
first.

#### D3.5 Upstreams

A list of upstreams, each a row with: name and note; address; disclosure
mode as a badge with its meaning in words ("via: another cuttle
instance", "none: discloses no intermediary"); the live state from
`cuttle status` (up, down for 42 s with 17 failed fast, probing) and the
last error; usage ("Rule `restricted`, rule `docs`, proxy.remote.default")
with links; and its 24-hour requests and bytes.

The editor is a dialog over the list, or a page without the script:

- Name, Note, Address (`host:port`).
- Disclosure, as two radio cards with no default selection, because
  either wrong default fails ([0003 D1][d3-1]): "via — another cuttle
  instance; adds one Via entry for loop detection" and "none — an egress
  proxy or any proxy that must not see an intermediary".
- Timeouts (connect, response, unreachable hold) in a collapsed "Timeouts"
  group whose summary shows the values in force.
- Interfaces: "System routing" or "Only these interfaces, in this order".

Renaming an upstream also changes every `upstream:<old>` action and
default in the same save (D7); the review lists them. An upstream that
an action uses has no Delete; its menu says "Used by 2 rules and
proxy.remote.default" with links. Removing those uses first enables
Delete.

Empty state: "No upstreams. Add one to send some destinations through
another cuttle instance or an egress proxy", with "Add upstream".

#### D3.6 Interfaces

Two sections:

- **Defined interfaces**: name, note, state (online, demoted until a
  time, offline with the [0008 D4][d8-4] reason), the matched device, the
  identity keys in words ("hardware address", "device en0, Wi-Fi"),
  where the definition is used (proxy.interfaces position 1, rule
  `video`, upstream `gateway`), and its 24-hour bytes.
- **Detected on this Mac**: the rows of `cuttle interfaces` read through
  the same `iface.Source` ([0008 D15][d8-15]): device, kind, media,
  first IPv4 and IPv6 address, default route, hardware address, and the
  definition that matches it. A row without a definition has "Define…".
  The table reads the system when the page loads and has a "Refresh"
  button; it never runs another program.

"Define…" opens the editor prefilled with a suggested name (`wifi` for
Wi-Fi, `eth-a`, `eth-b`, … for wired, `if-<device>` otherwise) and the
identity that [0008 D2][d8-2] recommends: device and kind for built-in
Wi-Fi, the hardware address for everything else. The editor offers the
three identity choices (hardware address, device with a kind guard,
both) with one sentence each. Renaming updates every interface list in
the same save; a definition that a list uses has no Delete and names
its users.

Empty state: "No interfaces defined. Direct connections follow macOS
routing. Define the interfaces below to choose which ones cuttle uses",
followed by the detected table. If the table cannot be read, the section
says so with the error and the defined interfaces still show.

#### D3.7 Proxy, Remote clients, General

Settings pages of the same pattern: groups of fields, each field with a
label, a one-line hint written from the 0004 D3 meaning, and, for an
unset optional key, an empty input whose placeholder shows the default
("10s, the default"). The field-state rule of D4 decides what a save
writes: an untouched field leaves its key as the file has it, and
clearing a text field removes the key, so the default applies. Two keys
have an empty value that differs from unset, `proxy.remote.ports` and
`admin.listen`, so they get explicit choices instead of an empty field.
Restart-only keys carry a "Restart" badge with the hint "Applies at the
next start".

- **Proxy**: Listeners (list input, Restart); Default route for local
  clients (action control as in D3.4); Timeouts: connect, response, idle
  ("0s keeps idle tunnels open"); Connection limit; Interfaces: the
  default list editor, attempt timeout, and demotion. The interface
  fields are collapsed, with a one-line summary, when no interface is
  defined.
- **Remote clients**: Sources (CIDR list input); Ports as three choices,
  "Default (80 and 443)", "These ports" with a list input, and "No ports
  (remote clients refused)", which writes `ports = []`; Default route for
  remote clients (Direct is disabled unless "Allow direct" is on, with
  the reason), Allow direct with the [0003 D10][d3-10] consequence in one
  sentence, and the per-source limit. When every listener is loopback
  and no source is set, the page opens with "Remote clients are off:
  every listener is a loopback address. Add a listener on a private link
  address on the Proxy page, then allow its sources here."
- **General**: Instance name, with the note that it appears in `Via` and
  that the other instances of a chain see it; Admin listener (Restart)
  as three choices, "Default (127.0.0.1:3129)", "On at" with an `IP:port`
  field, and "Disabled", which writes `admin.listen = ""` and whose
  review warns "After the next start this page is no longer served";
  Statistics
  retention and hosts per hour; Log level as a segmented control.

#### D3.8 The interface list editor

One component for `proxy.interfaces`, a rule's `interfaces`, and an
upstream's `interfaces`: the chosen interfaces in priority order, each
with its state badge and Move up, Move down, and Remove buttons, and an
"Add interface" select of the remaining definitions. The hint says
"Requests use only these interfaces, in this order." Without
definitions, the editor shows a link to the Interfaces page instead.

#### D3.9 Account

- **Password**: current password, new password, and confirmation, with
  the 12–128 byte rule as the hint. D12 defines the checks.
- **Sessions**: "This browser" and the number of other sessions, with
  "Sign out other sessions".
- The password file path and when it last changed.

#### D3.10 Advanced

The M2 settings page, restyled, with two additions:

- The primary button becomes "Review changes", which opens the review
  (D7) for the edited text. "Validate" stays. `POST /settings`, the
  direct save, stays for compatibility and for the M2 tests; the page no
  longer offers it as a button.
- **Previous version**: when `<config>.bak` exists and passes the D12
  checks, the page shows its size and modification time, "Compare with
  current" (the diff from the current file to the previous version),
  and "Restore", which opens
  the review with the `.bak` content as the candidate. Applying it writes
  the current file to `.bak`, so a restore can itself be undone.

Both stay usable when the file on disk has errors (D3.13), as the M2 page
does ([0005 D10][d5-10]); they are how the operator fixes such a file
from the UI (D7).

#### D3.11 Review

The review is the one step before every write. With the script it is a
dialog over the page; without it, a page. Content, top to bottom:

1. A title that names the edit, such as "Review: change rule
   `restricted`".
2. The change summary (D7): one line per changed entity or key, such as
   "Rule `restricted`: domains +1", "Moved rule `ads` from 5 to 2",
   "Upstream renamed `lab` → `lab-b`; 2 rules updated".
3. Notices, when they apply: the file changed since the form opened
   (D7); the file is not yet in canonical form, so the comments written
   by hand (N comment lines and M end-of-line comments) are removed and
   the formatting changes; the file holds changes that are not running
   yet, which this apply also loads; restart-only keys that change;
   validator warnings.
4. The unified diff from the current file to the candidate: the line
   numbers of both, `+` and `−` markers as well as color, and
   unchanged stretches longer than 6 lines collapsed into a
   "12 unchanged lines" disclosure (`<details>`, so it needs no script).
5. Buttons: "Apply changes" (primary), "Back to editing", and "Cancel".

After "Apply changes", the browser returns to the page the edit started
on, where a toast reports the result (D7).

#### D3.12 Route a host

"Route…" on the overview, domains, and live pages opens `/rules/new`
with the host prefilled (a dialog with the script, a page without). It
shows:

- **Currently**: how the running configuration routes the host for a
  local client, as in D8.
- **Match**: a choice between the exact host and each parent domain down
  to two labels, for example `cdn.video.example.com` (this host only),
  `video.example.com` and its subdomains, `example.com` and its
  subdomains. The default is the exact host. cuttle has no list of
  public suffixes, so it cannot tell a parent such as `co.uk` or
  `github.io` from a registrable domain; the operator picks a parent
  knowingly. An IP literal offers its `/32` or `/128`.
- **Route**: Direct, an upstream, or Block. None is preselected, and the
  host's current route is disabled.
- **Where**: "New rule, first in the chain" (default) or "Add to rule
  `…`", listing the rules whose action equals the chosen route and that
  have no `ports`.

So routing a host takes four activations: Route…, the route, Review
changes, and Apply changes. The match and the place have defaults.

#### D3.13 Common states

| State | Where | Presentation |
| --- | --- | --- |
| Empty | Lists and tables | One sentence on what the list holds and what an empty list means for traffic, and the primary action |
| Loading | Script requests only: live test, live check, review dialog | The control keeps its content and shows a small inline spinner after 150 ms; pages themselves render on the server |
| Request failed | Script requests | The panel says "Could not reach cuttle. Check that it is running, then try again" with a Retry button; the form keeps its values |
| Field errors | Forms | Under each field, with `aria-invalid`; an error summary at the top that links to each field and takes focus after a full-page submit |
| Unsaved changes | Forms and the rules order | A sticky bar at the bottom with Discard and Review changes; leaving the page asks first |
| Conflict | Review and apply | D7 |
| Restart required | Shell banner, review, toast | The keys, and the command `brew services restart cuttle` |
| Reload failed | Toast and shell banner | The validator's messages; the running configuration stays |
| File has errors | Every configuration page | A banner with the errors and links to Advanced and, when the previous version is valid, to restoring it. Form pages are read-only and show the running configuration, and every draft intent of a form op answers `409` (D7). Advanced and restore stay usable, so the operator can fix the file from the UI |
| File differs from running | Shell banner | "The file changed since cuttle loaded it" with "Reload now", which runs the [0004 D6][d4-6] reload |
| Statistics unavailable | Traffic pages, traffic columns | Traffic pages show the reason once; configuration pages hide traffic columns and show "Statistics unavailable" in one muted line |
| Admin listener disabled next start | Review, General | A warning in the review before applying |

### D4. Field catalogue

Every key of [0004 D3][d4-3] has exactly one form field. The T0 test
`TestFormCoverage` (W17, W22) compares this table with the schema.

**Field-state rule.** It applies to the fields of an existing entity
and of a settings page. A new entity (Add, Duplicate, Define…, Route a
host) has no key in the file yet, so its save writes every field that
holds a value, prefilled values included; Define… keeps the `kind` it
prefilled, and an empty field stays unset.

Each field carries, in a hidden input, its rendered value. Only a `GET`
takes it from the file: a set key renders its value, and an unset key
its default, as an empty text field with the default as placeholder, or
as the default choice of a checkbox, segmented control, or explicit
choice. Every later render of the same form (Back to editing, `check`,
a `422` review, Review again after a `409`, the normal submit when the
script fails) and the review page carry the posted rendered values
forward unchanged; they never recompute them from the controls or from
the current file. So a change made before a re-render still counts as a
change at the next save, and a key that another writer changed while
the form was open is not mistaken for the operator's edit. Rendered and
posted values are compared parsed, not as raw text: list inputs after
splitting on newlines, commas, and spaces, text after turning CRLF into
LF, and integers as numbers. On a save:

- a field posted with its rendered value leaves its key as the current
  file has it, set or unset, so untouched checkboxes and segmented
  controls never turn an unset key into an explicit default, and a key
  that another writer changed meanwhile keeps that change (D7);
- a changed field sets its key to the posted value;
- a text field or list input that the operator emptied removes its key,
  and so does a checkbox cleared to `false`, since every boolean key
  defaults to `false`;
- the explicit choices of `proxy.remote.ports` and `admin.listen` map as
  the table says.

The last column says what an empty, cleared, or unset field means.

| Key | Page | Control | Empty, cleared, unset |
| --- | --- | --- | --- |
| `name` | General | Text | Required; empty gets the validator's "required" |
| `proxy.listen` | Proxy | List input of `IP:port`, Restart | Unset is `["127.0.0.1:3128"]`; `[]` is an error, so an emptied list unsets |
| `proxy.default` | Proxy | Action control | Required; the control always has a value |
| `proxy.connect_timeout`, `response_timeout`, `idle_timeout` | Proxy | Duration text | Cleared unsets (10s, 60s, 1h); a typed `"0s"` keeps idle tunnels open |
| `proxy.max_connections` | Proxy | Integer | Cleared unsets (1024) |
| `proxy.interfaces` | Proxy | Interface list editor | `[]` and unset both mean system routing; an emptied list unsets |
| `proxy.interface_attempt_timeout`, `interface_demotion` | Proxy | Duration text | Cleared unsets (3s, 30s); a typed `"0s"` disables demotion |
| `proxy.remote.sources` | Remote clients | List input of CIDRs | `[]` and unset both mean no remote source; an emptied list unsets |
| `proxy.remote.ports` | Remote clients | Three choices: Default, These ports (list input), No ports | Default unsets (80 and 443); No ports writes `[]`, which refuses remote clients on every port; These ports with an empty list is a form error, "List at least one port, or choose No ports" |
| `proxy.remote.default` | Remote clients | Action control | Unset is `block`; untouched stays unset |
| `proxy.remote.allow_direct` | Remote clients | Checkbox | Unset is `false`; cleared unsets |
| `proxy.remote.max_connections_per_source` | Remote clients | Integer | Cleared unsets (128) |
| `[[interface]]` `name`, `note`, `mac`, `device`, `kind` | Interfaces | Text, text, identity choice with two text fields, select | `name` required; the identity choice decides which of `mac` and `device` are set; `kind` "No guard" unsets; a cleared note unsets |
| `[[upstream]]` `name`, `note`, `address` | Upstreams | Text | `name` and `address` required; a cleared note unsets |
| `[[upstream]]` `disclosure` | Upstreams | Radio cards, no default | Required; no choice gets the validator's "required" |
| `[[upstream]]` timeouts | Upstreams | Duration text | Cleared unsets (10s, 60s, 5s); a typed `"0s"` disables the hold |
| `[[upstream]]` `interfaces` | Upstreams | Interface list editor | "System routing" unsets; "Only these" with an empty list gets the validator's empty-list error |
| `[[rule]]` `name`, `note` | Rules | Text | `name` required and always written (D6); a cleared note unsets |
| `[[rule]]` `hosts`, `domains`, `cidrs`, `ports` | Rules | List inputs | An emptied list unsets, because the validator refuses `[]`, which would widen the rule |
| `[[rule]]` `action` | Rules | Action control | Required |
| `[[rule]]` `allow_loopback`, `allow_private` | Rules | Checkboxes | Unset is `false`; cleared unsets |
| `[[rule]]` `interfaces` | Rules | Interface list editor | "Use the default list" unsets; "Only these" with an empty list gets the validator's empty-list error |
| `admin.listen` | General | Three choices: Default, On at (text), Disabled; Restart | Default unsets (127.0.0.1:3129); Disabled writes `""`, which disables the admin UI; On at with an empty field is a form error, "Enter an IP:port, or choose Default or Disabled" |
| `stats.retention_days`, `max_hosts_per_hour` | General | Integer | Cleared unsets (30, 1000) |
| `log.level` | General | Segmented control | Unset is `info`; untouched stays unset |

Durations stay text, because the schema's Go duration syntax (`"1m30s"`)
is what the hint shows and what the validator reads.

### D5. Convenience features

Included, with the reason each pays for itself:

| Feature | Reason |
| --- | --- |
| List inputs that accept a pasted list | Rule patterns usually arrive as a list from a browser's network panel or another config. The base control is a textarea with one entry per line; the script turns it into chips and splits pasted text on newlines, commas, and spaces. The server splits the same way, so both paths agree. |
| Inline field errors from validator key paths | The validator already names `rule[3].domains[2]`; showing the message on the entry turns a 30-line error list into a fix in place. |
| Live check while editing | The script posts the draft 500 ms after typing stops and shows errors and warnings in place. The server runs the real validator, so nothing is duplicated. |
| Test a URL, live, in the list and the editor | First-match order is the main source of surprise; seeing which rule wins, and that an earlier rule shadows a draft, removes the guess. It is the same function as `cuttle explain` (D8). |
| The rule chain with default row | It shows the evaluation order and what happens when nothing matches, which a plain table hides. |
| Drag and keyboard reordering with one review | Order is semantics; several moves should cost one save. |
| Filter over rules | Rule lists grow to dozens; the filter also finds which rule holds a pattern. |
| Action, state, and disclosure badges | The three routes and the up/down states are the vocabulary of every page; color plus text makes them scannable. |
| Usage hints and 24-hour traffic | They answer "is this rule or upstream still used?" before a delete, from data cuttle already has (D9). |
| Rename with reference update | Without it, renaming an upstream or interface takes several saves through invalid states. |
| Guarded deletes | An upstream or interface still in use cannot be deleted; the menu names its users. A rule's delete shows its traffic and where that traffic goes next. |
| Route a host from traffic pages | The commonest edit starts from a host seen in traffic; four activations replace copying a name into a form. |
| Review with change summary, diff, and notices | The user's decision; the summary makes reorders and renames readable where a text diff is noisy. |
| Restart-required and file-differs banners, reload result toasts | The operator must know when a saved change is not yet in force. |
| Restore the previous version | `.bak` already exists; offering it makes every form save one click from undone. |
| Notes on rules, upstreams, and interfaces | Hand comments no longer survive a form save; a one-line note gives back the annotation in a form the writer keeps (D6). |
| Defaults as placeholders, clearing restores the default | The operator sees the value in force without the file writing every default. `proxy.remote.ports` and `admin.listen` have Default as an explicit choice instead, because their empty value means something else (D4). |
| Unsaved-changes guard | Losing an edit to a navigation is the commonest form failure. |
| `/` focuses the filter or the test field; Command-Enter reviews | Two shortcuts for the two most frequent actions; no others. |

Left out:

| Feature | Reason |
| --- | --- |
| A staged change set across pages | Needs server-side draft state that a restart loses, and makes conflicts harder to explain; per-page saves with review are enough for one operator. |
| Auto-save | Every write must pass the review the user asked for. |
| Undo history beyond `.bak` | `.bak` plus a dotfile repository covers it; more copies of a file that names private hosts add exposure. |
| A manual theme switch | The pages follow the macOS appearance; a switch adds stored state for no gain on a single-user Mac. |
| A side-by-side diff | It does not fit a phone; the unified view with both line numbers carries the same information. |
| Bulk edit and import | A paste into the Advanced editor does this already. |
| Charts beyond CSS bars | [0005 D9][d5-9] still holds: CSS bars in 5 % steps and the stacked route bar answer the questions without a chart library. |
| Per-rule hit counters in memory | Statistics per rule (D9) cover it with the 10-second lag the dashboard already has. |

### D6. Configuration model and canonical writer

**Model.** `config.Doc` mirrors the [0004 D3][d4-3] schema. Every
optional key is held as set or unset, with its value as written: strings
as strings, durations as their strings, lists as lists, integers as
`int64`, booleans as `bool`. Arrays of tables keep their order. A `Doc`
is built only from a file that `config.Parse` accepts, from the same
decoded TOML map, so every value has its schema type. A rule without
`name` gets the name `rule-<n>` that the validator gave it, written
explicitly, so that a later reorder does not rename it.

**Writer.** `(*Doc).Encode() []byte` writes the canonical form:

- UTF-8, LF line ends, two-space indentation only inside multi-line
  arrays, no tabs, no trailing spaces, one final newline, one empty line
  between tables.
- Order: the header comment; `name`; `[proxy]`; `[proxy.remote]`;
  `[admin]`; `[stats]`; `[log]`; the `[[interface]]` entries; the
  `[[upstream]]` entries; the `[[rule]]` entries. Definitions come before
  their uses, and the rules, the longest and most edited part, come last.
- Keys within a table in the order of the 0004 D3 tables, with `note`
  right after `name`. Only keys that the model sets are written; a table
  with no set key is left out.
- Strings as TOML basic strings: `"` and `\` escaped, control characters
  (U+0000–U+001F and U+007F, which TOML forbids raw in basic strings) as
  `\b`, `\t`, `\n`, `\f`, `\r`, or `\uXXXX`, other characters as UTF-8.
  No literal or multi-line strings, no inline tables, no dotted keys.
- Integers in decimal without separators; booleans as `true` and
  `false`.
- An array on one line when the whole line fits in 80 columns, otherwise
  one element per line with a trailing comma:

  ```toml
  domains = [
    "ads.example.net",
    "tracker.example",
  ]
  ```

Generated comments, the complete set, each written only when its table
or group is present:

```toml
# cuttle configuration. The admin UI writes this file in this form and
# replaces comments with these generated ones; the previous version is
# kept beside it with a .bak suffix. Check hand edits with `cuttle check`
# and apply them with `cuttle reload`.

# Listeners and the route for requests that no rule matches.
[proxy]

# Clients on other machines: allowed sources, ports, and their default.
[proxy.remote]

# Admin UI; set the first password with `cuttle admin passwd`.
[admin]

# Network interfaces, named for interface lists; `cuttle interfaces`
# lists this Mac's interfaces.
[[interface]]

# Upstream proxies. disclosure: "via" for another cuttle instance,
# "none" for an egress proxy.
[[upstream]]

# Rules, tried in this order; the first match wins.
[[rule]]
```

The header names no path or file name, so it reads the same for every
configuration path. No comment holds a time, a host name, or anything
else that changes from save to save, so writing the same model twice
gives the same bytes.

**`note`**, a new optional key of `[[rule]]`, `[[upstream]]`, and
`[[interface]]`, added by the user's decision of 2026-11-26: a string of
at most 120 characters, counted as Unicode code points (so at most 480
bytes of UTF-8), with no control characters (U+0000–U+001F and U+007F),
shown in lists and editors and never in logs, status, or statistics.
The validator rejects a longer note or one with a control character,
and its message does not repeat the value. It applies on
reload and changes no behavior; a changed note alone keeps an upstream's
transport (the comparison of [0004 D6][d4-6] step 3 ignores it). A
binary older than the one that adds `note`, such as v0.1.0, refuses a
file with a note as an unknown key, so the release notes of the version
that adds it say so for operators who downgrade.

**Unknown and deprecated keys.** The validator refuses unknown keys, so a
`Doc` never meets one, and a file with one cannot be edited in a form
until it is fixed in Advanced. No key is deprecated today. When a later
schema change renames a key, the validator accepts the old name with a
warning, `Doc` reads it into the new key, and the next form save writes
the new name; the review lists it under the change summary.

**Properties**, each tested in M4.1 (W1–W3):

- *Same configuration:* for every file `f` that `Parse` accepts,
  `Parse(Encode(DocOf(f)))` equals `Parse(f)` apart from `SHA256`.
- *Canonical fixed point:* `Encode(DocOf(Encode(DocOf(f))))` equals
  `Encode(DocOf(f))` byte for byte.
- *Model fidelity:* `DocOf(Encode(d))` equals `d` for every `Doc` that
  `DocOf` returned, so set and unset keys survive a save.

An encoded file larger than the 256 KiB limit is refused at review with
the size, as the text editor refuses it.

### D7. Edit and save pipeline

**Endpoints.** Every `POST` needs a session, the session's CSRF token,
and passes `http.CrossOriginProtection` ([0005 D8][d5-8]).

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/rules`, `/rules/new`, `/rules/{name}` | Rules list (`?q=` filter, `?test=` and `source=` for Test a URL), new rule (`?host=` prefill), rule editor |
| `GET` | `/upstreams`, `/upstreams/new`, `/upstreams/{name}` | List and editor |
| `GET` | `/interfaces`, `/interfaces/new`, `/interfaces/{name}` | Definitions, detected table, editor (`?device=` prefill) |
| `GET` | `/proxy`, `/remote`, `/general`, `/account` | Settings forms |
| `GET` | `/settings`, `/settings/backup` | Advanced editor; the previous version and its diff |
| `POST` | `/config/draft` | Check, test, review, or return to editing a draft; never writes |
| `POST` | `/config/apply` | Write a reviewed candidate and reload |
| `POST` | `/config/reload` | Reload the file as it is ([0004 D6][d4-6]) |
| `POST` | `/account/password` | Change the password (D12) |
| `POST` | `/account/sessions` | End every other session |
| `POST` | `/settings/validate`, `/settings` | Unchanged ([0005 D10][d5-10]) |
| `GET` | `/static/admin.css`, `/static/admin.js` | Embedded assets |

A path name in `{name}` must match its schema pattern; another value gets
`404`.

**Edits.** Each form posts the whole state of what it edits, the
rendered value of each field (D4), the `op` that names the edit, and the
file's `base`, the SHA-256 of the file that the `GET` rendered the form
from. Re-renders and the review page carry the rendered values and
`base` forward unchanged. The ops:

| `op` | Edit of the `Doc` |
| --- | --- |
| `rule.save` | Replace the rule named `target` with the posted fields, or insert a new one at `position` |
| `rule.delete` | Remove the rule `target` |
| `rules.order` | Reorder: the posted list must name every rule exactly once |
| `upstream.save`, `interface.save` | Replace or add; a changed name rewrites every reference (`upstream:<old>` in actions and defaults, or the name in every interface list) |
| `upstream.delete`, `interface.delete` | Remove; refused with the users listed while anything refers to it |
| `proxy.save`, `remote.save`, `general.save` | Replace the keys of that page |
| `route.host` | D3.12: insert a rule, or append the pattern to an existing rule |
| `text` | The Advanced editor's text is the candidate as written |
| `restore` | The `.bak` content is the candidate as written |

**Draft intents.** `POST /config/draft` carries `intent`:

- `edit` renders the form from the posted values (Back to editing);
- `check` validates and renders the form, or only its issue list for the
  script, with errors and warnings in place;
- `test` runs Test a URL against the candidate (D8);
- `review` validates and, when there is no error, renders the review.

Every intent runs the same steps. The `text` and `restore` ops skip the
steps marked *form ops*: their candidate is text, so no `Doc` is built,
and their review shows the plain-text diff.

1. Read the file. If it cannot be read, answer `409` with the read error
   for every op, as the Advanced page shows it. If it is invalid, a form
   op answers `409` with the D3.13 "file has errors" state, because a form
   edit needs a `Doc` of the current file; `text` and `restore` go on,
   since they are how the operator replaces an invalid file.
2. *Form ops:* build the `Doc` and apply the op. An op whose target no
   longer exists (`rule.save` of a rule deleted elsewhere, `rules.order`
   whose names no longer match the rules) answers `409` with the form
   kept and "Rule `x` no longer exists; it was changed outside this
   page", and a link that reloads the page.
3. *Form ops:* apply the D4 field-state rule, and parse integer fields
   and the two explicit choices of D4. A value that is not an integer
   becomes an issue with the validator's key path and wording ("must be
   an integer"); an explicit choice without its value becomes an issue
   with that key path and the form's own message of D4. Neither writes
   or calls the validator.
4. Build the candidate, `Encode` of the edited `Doc` for a form op, the
   posted text with CRLF turned into LF for `text`, or the `.bak` content
   with CRLF turned into LF for `restore`, and run `config.Parse` on it.
   The review shows this normalized text, the same bytes that apply
   writes. That report is the only
   judgment.
5. Map each issue to the form (below), or, for `text` and `restore`, list
   the issues with their lines as the Advanced page does, and render.

Nothing on the draft or apply path refuses a candidate because the
current file is invalid: `POST /config/apply` and `POST /settings` check
only the size limit, the candidate's validity, and `base`
([0005 D10][d5-10]).

**Key paths to fields.** The candidate is canonical, so `rule[n]` is the
`n`-th rule of the `Doc`, which the page knows by name. An issue key is
split into the entity (`rule[3]`, `upstream[1]`, `interface[2]`,
`proxy`, `proxy.remote`, `admin`, `stats`, `log`, or the top level), the
key, and the entry index (`domains[2]`). Issues of the edited entity go
under their field, and an entry index marks that entry of a list input
("Entry 2: …"). Issues of other entities, such as a warning on a later
rule that the edited rule now shadows, go in a list under the error
summary with a link to that entity. The mapping is one table-driven
function with a T0 test (W5).

**Concurrency.** The edit applies to the file as it is now, never to a
stale copy, and the review always compares against the current file.

| Situation | Behavior |
| --- | --- |
| `base` equals the current file | Normal review |
| `base` differs, and every changed field's key, of the existing entity or of the settings page, still has its rendered value in the current file | The review shows the notice "The file changed after you opened this page. The changes below are yours; the other change stays." Untouched fields keep the other change (D4) |
| `base` differs, and the key of a changed field also changed elsewhere | The review shows the warning "`restricted`: domains was also changed elsewhere. Applying replaces that change with yours; the diff shows what is lost." For a new entity the other change always stays |
| `text` or `restore`, and `base` differs | The review shows the warning "The file changed after you opened this page. Applying replaces that change with this text; the diff shows what is lost." |
| The edited entity no longer exists | `409` at the draft step (step 2) |
| The file changes between review and apply | `POST /config/apply` answers `409` as `/settings` does, and shows a "Review again" button that rebuilds the review from the posted fields and their rendered values |

**Apply.** The review's form posts `text` (the candidate exactly as
reviewed), `reviewed` (the digest of the file the review compared
against, which the write checks as `ExpectSHA256`, as `/settings` checks
its `base`), the form's `op`, fields, rendered values, and `base`, so
that Review again can rebuild the review, and `return`.
`POST /config/apply` and `POST /settings` share one function that runs
the [0005 D10][d5-10] steps: size limit, validation, `WriteAtomic` with `ExpectSHA256` and the
backup, and reload, under the settings mutex, one save at a time. It
first turns CRLF line ends into LF, as `submitted()` does for
`/settings` today, because the review's hidden `text` field arrives with
CRLF. It logs `admin settings saved` with the digest and the `op`. On success it
answers `303` to `return`, and the next page shows a toast:

- "Saved and reloaded." with the change summary's first line;
- "Saved and reloaded. Restart required for proxy.listen." with the
  restart command;
- "Saved and reloaded with 2 warnings." with the warnings;
- "Saved, but the reload refused the file; the running configuration is
  unchanged." with the errors, which also sets the shell banner.

The toast's text is a one-time flash stored in the session, consumed by
the next page that renders it. Success toasts close after 8 s with the
script; failure toasts stay until closed. `return` must be one of the
configuration or traffic page paths with an optional query that
re-encodes cleanly; anything else gets `400`. An edit that renamed or
deleted the entity of an editor path, such as a rename started at
`/rules/{old}`, returns to that entity's list (`/rules`, `/upstreams`,
or `/interfaces`) instead.

This stage also settles [OI-0006][oi-6] U-R1: a directory sync that fails
after the rename counts as saved; the write logs a warning, the reload
runs, and the toast says "Saved; the directory could not be synced".

**Restart required at review.** The review compares the candidate,
through `config.RestartRequired`, with the configuration the instance
started with, as the reload already does for `restart_required`
([0004 D6][d4-6] step 5; `startCfg` in `cmd/cuttle/serve.go`), and lists
the keys that would need a restart before the operator applies.
`Backend` gains `StartConfig() *config.Config` for it. A comparison with
the running configuration would miss a key that an earlier save already
changed and would list a key changed back to its start value.

**Change summary.** `config.Summarize(old, new *Doc, hint)` compares two
models: rules, upstreams, and interfaces by name (added, removed, keys
changed, renamed when the op's hint says so); rule moves from the
longest common subsequence of the name lists, so that one move reads as
one line; and each changed key of the singleton tables with old and new
value. A summary line never shows a MAC address; it says "hardware
address changed". When the current file is not canonical
(`Encode(DocOf(file))` differs from the file), the review adds the
notice "This save rewrites the file in canonical form: N comment lines
and M end-of-line comments written by hand are removed."
For `text` and `restore`, which build no `Doc`, the summary is one line,
"Edited as text: N lines added, M removed" or "Restores the previous
version saved at <time>", and when the current file is invalid the
review adds the notice "The current file has errors; applying replaces
it."

**Diff.** `internal/textdiff` computes a Myers line diff of the current
file and the candidate and returns hunks with three lines of context.
Both inputs are at most 256 KiB, so the cost is bounded.

### D8. Test a URL

- The target parsing and the decision of `cuttle explain` move from
  `cmd/cuttle` into `config.Explain(cfg *Config, target string, source
  policy.SourceClass) (Explanation, error)`. `cuttle explain` prints the
  explanation as it does now, plus the `address` line below, and the
  admin renders the same fields: the normalized target, kind, and
  source; the rule with its position, or `default`, `remote-default`,
  `port`, or `remote-direct` with its reason; the route with the
  upstream's address and disclosure mode or the direct flags; the
  interface list in force with its source ([0008 D15][d8-15]); and the
  address verdict for literals.
- Inputs: `http://…` is a plain-HTTP request; `https://…` a tunnel to its
  port, 443 by default; `host:port` a tunnel; a bare host is read as
  `https://host`, and the result says so. Anything `cuttle explain`
  refuses gets its message.
- On the rules page the test uses the file's configuration; in an editor
  it uses the candidate. A candidate with errors gives "Fix the errors to
  test this draft" instead of a result.
- **Literal addresses.** When the route is `direct` and the target is an
  IP literal, `config.Explain` classifies the address with the
  [0003 D11][d3-11] table, as the dial would, and adds an `address` line:
  `unusable` addresses (link-local, multicast, and the rest) are refused
  for every client; `loopback` needs the matched rule's `allow_loopback`;
  a remote client with `allow_direct` reaches only `public` addresses. A
  refused literal reads "refused: <class> address, 502
  destination_ip_prohibited" ([0002 D6][d2-6]), so
  `http://169.254.169.254/` no longer looks like an allowed direct
  route. The check needs no DNS. It does not know this Mac's own
  addresses (`self`), because `cuttle explain` reads only the file, and
  for a name on a direct route the line says that the addresses are
  checked after resolution. `cuttle explain` prints the same line.
- The result also adds the upstream's live state, so "upstream gateway,
  down for 42 s" warns that a matching request fails now. It never
  resolves the name and never connects, as `cuttle explain` does not.

### D9. Usage and traffic hints

| Hint | Source |
| --- | --- |
| Which rules and defaults use an upstream; which lists use an interface | The `Doc` |
| Upstream and interface states | `Backend.Status()`, as on the overview |
| Requests and bytes per rule, last 24 hours | New `stats.RuleTotals(ctx, f)`: `host_hour` grouped by `rule` |
| Requests and bytes per upstream and interface | `RouteTotals` and `InterfaceTotals` |
| Top hosts of a rule | `Domains` with the new `Filter.Rule` |

Configuration pages use the 24-hour preset and link to `/domains` with
the matching filter. Statistics are keyed by rule name, so a renamed rule
shows no traffic until it is used under its new name; the rename review
says so. Without statistics the hints are left out (D3.13).

### D10. JavaScript

- One file, `/static/admin.js`, embedded like the stylesheet and loaded
  with `<script src="/static/admin.js" defer>` on session pages. Plain
  modern JavaScript as Chrome and Safari of the last two years run it,
  with no framework, no build step, no `eval`, no `new Function`, no
  inline handlers, and no storage APIs. The CSP of [0005 D5][d5-5] stays
  as it is: `default-src 'self'` already allows the script, and nothing
  needs inline script or style.
- **Every flow works without the script.** The forms submit to the same
  endpoints, the review is a page, reordering uses position fields, list
  inputs are textareas, the menu is a `<details>` element, and collapsed
  groups are `<details>` elements. Reasons: the T1 tests drive every flow
  with Go's HTTP client, so acceptance is deterministic and fast without
  a browser; a script that fails to load, or a Safari quirk, cannot block
  the operator from fixing routing; and the enhancements stay small
  because the server already renders every state. The cost is the
  position-field fallback and a full page per review, both simple.
- What the script adds: chips for list inputs with paste splitting; the
  live check and live test (debounced 500 ms, the latest request wins
  and earlier responses are dropped); drag and keyboard reordering;
  dialogs for the review, editors, and Route a host (`<dialog>` with
  `showModal`); toasts that close; the unsaved-changes guard; the filter
  as you type; the two shortcuts.
- **Fragments, not JSON.** A script request sends the header
  `Cuttle-Fragment: 1`, and the server renders only the named template
  block of the same page (issue list, test result, review, editor), so
  one template serves both paths and `html/template` escapes everything.
  The script parses the response with `DOMParser`, takes the fragment's
  root element, and replaces the target's children; it never inserts
  response text as HTML in any other way and never runs scripts from a
  response. Responses carry `Vary: Cuttle-Fragment`. Requests use
  `fetch` with `credentials: "same-origin"` and post the form's own
  `FormData`, which includes the CSRF token.
- A failed request shows the D3.13 request-failed state; the script then
  leaves the form to a normal submit.

### D11. Visual system

**Direction.** A calm instrument for one operator: quiet neutral
surfaces, ink-dark type, and color reserved for meaning. Three route
colors (direct, upstream, block) are the visual vocabulary of the whole
product: route badges, the overview's stacked bar, the chain's nodes,
and traffic bars all use them, so a color means the same route
everywhere. The one memorable element is the rule chain (D3.3), whose
line and nodes carry the first-match order and light up the path to the
matching rule when the operator tests a URL. Everything else stays
disciplined: lists with hairline dividers rather than a card per item,
panels only for groups of fields, dialogs, and the test result. Primary
buttons are ink-filled rather than colored, so that color always means a
route or a state.

**Color tokens.** Defined as custom properties on `:root`, redefined for
dark appearance under `prefers-color-scheme: dark`.

| Token | Light | Dark | Use |
| --- | --- | --- | --- |
| `--paper` | `#F3F5F4` | `#0D1316` | Page background |
| `--surface` | `#FFFFFF` | `#141B1F` | Panels, inputs, dialogs, sidebar |
| `--surface-2` | `#EBEFED` | `#1B2429` | Hover, code, table head, diff context |
| `--ink` | `#13202A` | `#E5EBE9` | Text, primary buttons, focus ring |
| `--ink-2` | `#3B4853` | `#BBC6CA` | Secondary text |
| `--muted` | `#5F6B75` | `#8D9AA2` | Hints, captions, placeholders |
| `--line` | `#D6DDD9` | `#263138` | Dividers, input borders |
| `--line-strong` | `#848F8A` | `#5E6C74` | Field borders, segmented controls |
| `--direct` | `#0B6E7F` | `#4CBAC7` | Direct route |
| `--upstream` | `#4A45BF` | `#A39CFF` | Upstream routes |
| `--block` | `#B3302A` | `#FF7F72` | Block route, errors, danger |
| `--warn` | `#8A5600` | `#E6AE4C` | Warnings, probing, demoted, restart |
| `--ok` | `#28743A` | `#5EC57B` | Up, online, success |
| `--direct-soft`, `--upstream-soft`, `--block-soft`, `--warn-soft`, `--ok-soft` | `#ECF3F5`, `#F0F0FA`, `#F9EEEE`, `#FAF1DF`, `#EEF4EF` | `#1E383D`, `#2E3247`, `#3E2D2E`, `#3A3627`, `#213A30` | Badge, banner, and diff-line backgrounds; the hue's own token is the text color on them |

Text tokens reach 4.5:1 against `--paper`, `--surface`, and
`--surface-2`; each hue token reaches 4.5:1 on its soft background, so
badge text is readable; and the hue tokens and `--line-strong` reach 3:1
as UI colors. W13 checks the ratios from the stylesheet.

**Type.** The system stack only, no web fonts: `-apple-system,
BlinkMacSystemFont, "Helvetica Neue", system-ui, sans-serif`, and
`ui-monospace, "SF Mono", Menlo, monospace` for values the operator types
or copies (hosts, patterns, addresses, key paths, TOML, diff). The scale
follows the classic steps, in pixels: 12 caption, 13 small and table
detail, 14 body and controls, 16 lead, 18 section heading, 24 page
title. Weights 400, 500, and 600; the page title at 600 with −0.015 em
tracking; body line height 1.5, headings 1.25. Numbers use tabular
figures. Labels are sentence case; nothing is set in capitals.

**Space, radius, elevation, motion.**

- Spacing steps of 4 px: 4, 8, 12, 16, 24, 32, 48. Page side margin 32
  px on desktop, 16 px on a phone.
- Radius by role: 4 px for chips and badges, 6 px for controls, 10 px for
  panels and dialogs, round for state dots.
- Elevation: flat surfaces with a 1 px `--line` border; menus and toasts
  `0 1px 2px` plus `0 8px 24px` shadows of the ink at 6 % and 12 %;
  dialogs a `0 24px 64px` shadow at 24 % over a backdrop of the ink at
  40 %. Dark appearance adds the border instead of relying on shadow.
- Motion answers actions only: 120 ms for state changes, 160 ms for
  dialogs and toasts, and the 400 ms trace down the rule chain after a
  test. Under `prefers-reduced-motion: reduce` every transition is off.

**Components.** Sidebar navigation; page header; banners (info, warning,
error); toasts; buttons (primary ink, secondary outlined, quiet, danger
outlined in `--block`), icon buttons with labels; text, number, select,
and textarea fields with label, hint, default placeholder, and error;
the list input (chips over a textarea); the segmented action control;
radio cards; checkboxes; the interface list editor; route badges
(`direct`, `upstream gateway`, `block`), state badges (dot plus word),
the Restart badge, warning badges; pattern chips; data tables with
right-aligned tabular numbers and CSS bars in 5 % width classes; the rule
chain; the test result panel; dialogs; the diff view; empty states. Icons
are inline SVG symbols in the layout, used with `<use href="#…">`, so no
image file is fetched. The prototype shows each one.

**Responsive behavior.**

- At 1100 px and wider: sidebar, content up to 1120 px, and the editor's
  test panel beside the form.
- From 760 px to 1100 px: sidebar and content; the test panel moves below
  the form.
- Below 760 px: the top bar with the Menu disclosure; rule rows and table
  rows become stacked blocks that keep the chain line at the left; data
  tables keep the host and the main number and hide the rest behind the
  row; dialogs fill the screen; the unsaved-changes bar spans the width.
  At 390 px no page scrolls sideways; long hosts wrap anywhere.

**Accessibility.**

- Every control has a visible label; hints and errors are tied with
  `aria-describedby`, errors set `aria-invalid`, and the error summary is
  a list of links to the fields.
- Focus is always visible: a 2 px `--ink` outline with a 2 px offset.
  Focus order follows the visual order; dialogs trap focus and return it
  to the control that opened them; Escape closes them.
- Reordering by keyboard: the drag handle is a button named "Move rule
  `restricted`, position 3 of 7". Space or Enter picks the rule up, the
  arrow keys move it, Space or Enter drops it, and Escape cancels; a live
  region announces each position. The row menu offers the same moves.
- Test results, live checks, and toasts use polite live regions; failure
  toasts use `role="alert"`.
- Color is never the only signal: badges carry words, diff lines carry
  `+` and `−`, states carry a word next to the dot, and chain nodes
  differ in shape for tested, matched, and not reached.
- Targets are at least 24 by 24 px, 32 px on a phone. Pages work at 200 %
  zoom. `lang="en"` on every page.

### D12. Security and privacy

- **Every new `POST`** (`/config/draft`, `/config/apply`,
  `/config/reload`, `/account/password`, `/account/sessions`) goes
  through `authed` with the CSRF token and through
  `CrossOriginProtection`, as the M2 posts do. `GET` pages never change
  state; the `?host=` and `?device=` prefills only fill a form. The Host
  allowlist and the security headers of [0005 D5][d5-5] apply unchanged.
- **Escaping.** Every value from the file, from traffic (hosts in
  statistics and live tunnels, which web content can choose), from the
  interface table, and from validator messages (which repeat the
  operator's text) reaches the page through `html/template`'s contextual
  escaping, in pages and fragments alike. Prefills from a query string
  are re-validated before they fill a field: `?host=` as a host name or
  IP literal, and `?device=` as a BSD name (`^[a-z]+[0-9]+$`, at most 15
  bytes) that the detected table holds; another value fills nothing.
  The script inserts only parsed fragments (D10). Long values wrap and
  never widen the page.
- **Password change** (`POST /account/password`): fields `current`,
  `new`, and `confirm`.
  - It shares `loginMu`, the backoff, and the failure count of
    [0005 D8][d5-8] with the login, as the user decided on 2026-11-26:
    during a backoff wait it answers `429` with `Retry-After` and runs no
    Argon2id; a wrong current password
    answers `401` with "Invalid password." and counts as a failed login.
    So the form is no faster a guessing oracle than the login.
  - The new password must be 12–128 bytes and equal its confirmation;
    otherwise `422` with the message under the field, before any hash.
  - It hashes with the [0005 D6][d5-6] parameters and writes the file
    with `WritePasswordFile` (atomic, mode 0600). The digest change ends
    every session ([0005 D7][d5-7]); the server then starts a new session
    for this browser, with a new ID and CSRF token, and shows "Password
    changed. Other sessions were signed out." A success resets the
    shared failure count, as a successful login does. It logs
    `admin password changed` without either password.
  - Without a usable password file, login is disabled and the page cannot
    be reached, so the first password still comes only from
    `cuttle admin passwd`.
- **Sign out other sessions** removes every session except the current
  one.
- **The previous version.** `/settings/backup` and `restore` read
  `<config>.bak` beside the resolved target, the path that
  [0004 D7][d4-7] writes. They open it with `O_NOFOLLOW`, so a link is
  refused, check the open descriptor with `Fstat` (a regular file, owned
  by this user, not writable by group or others, at most 256 KiB), and
  read from that same descriptor, so nothing can be swapped in between
  the check and the read; otherwise the page says why. The restored content
  goes through the same validation, review, and apply. The diff and the
  `.bak` view show only text that the `/settings` editor of the same
  session already shows: the current file and the file that the previous
  save replaced. Nothing new is written to disk besides the existing
  `.bak` and the configuration file, both 0600.
- **MAC addresses and interface addresses.** The interfaces page shows
  the detected table, hardware addresses and addresses included, because
  the operator chooses from it, as from `cuttle interfaces`. Definitions,
  the review, and the Advanced editor show the file. Status, logs,
  counters, statistics, toasts, change summaries, and validator messages
  still never show a MAC ([0008 D15][d8-15], amended below). The page is
  served only to an authenticated session on loopback with
  `Cache-Control: no-store`.
- **Notes** never reach the log, status, or statistics.
- **Trust.** The operator who writes the configuration is trusted
  ([AGENTS.md](../../AGENTS.md)); the controls above protect against web
  content in the browser (cross-origin posts, rebinding, injected host
  names) and other local users (file modes, `.bak` checks).

### D13. Implementation outline

| Package or file | Change |
| --- | --- |
| `internal/config` | `doc.go` (`Doc`, `DocOf`), `encode.go` (writer and comments), `edit.go` (ops), `summary.go` (change summary), `explain.go` (moved from `cmd/cuttle`, with the literal-address check through `netx.Classify`), `note` in the validator, the U-R1 fix in `atomic.go` |
| `internal/textdiff` | Myers line diff and hunks |
| `internal/stats` | `RuleTotals`, `Filter.Rule` |
| `internal/admin` | `config.go` (draft and apply), `forms.go` (form ↔ `Doc` and issue mapping), one file per page group, flash in the session, `Backend.Interfaces() ([]iface.Link, error)` read through the instance's `iface.Source`, `Backend.StartConfig() *config.Config` for restart-required keys |
| `internal/admin/templates` | New layout and pages; fragments as named blocks |
| `internal/admin/static` | `admin.css` rewritten on the D11 tokens; `admin.js` |
| `cmd/cuttle` | `explain` uses `config.Explain`; `TestAdminChrome` extended |

No new module. The tokens and components land with the shell in M4.1, so
the M4.2 and M4.3 pages only compose them.

## Acceptance criteria

T1 tests run the admin server with the fake backend of
`internal/admin/backend_test.go` and a temporary configuration file
unless marked otherwise. "Review" means `POST /config/draft` with
`intent=review`.

### M4.1 — Configuration model, writer, and save path

| ID | Criterion | Check |
| --- | --- | --- |
| W1 | Same configuration: for every valid file in `internal/config/testdata`, the starter file, and a fuzz corpus seeded with them, `Parse(Encode(DocOf(f)))` equals `Parse(f)` apart from `SHA256`. | T0 `TestCanonicalRoundTrip`, `FuzzCanonical` |
| W2 | Canonical fixed point and model fidelity hold for the same inputs; a golden test fixes the order, the formatting, the 80-column array rule, and the generated comments of D6. | T0 `TestCanonicalFixedPoint`, `TestCanonicalGolden` |
| W3 | Strings with `"`, `\`, tab, U+007F, and non-ASCII characters survive the round trip, and U+007F is written escaped; a `note` of 120 non-ASCII characters is accepted, and one of 121 characters or with a control character is an error whose message does not contain the value; a note change keeps an upstream's transport on reload. | T0 `TestCanonicalEscaping`, `TestNoteValidation`; T1 `TestReloadNoteOnly` |
| W4 | Each op of D7 gives the expected `Doc`: insert at first, before, and last; replace; delete; reorder; upstream and interface renames rewrite every reference; deleting a used upstream or interface is refused with its users; `rules.order` with a missing or repeated name is refused. | T0 `TestDocEdits` |
| W5 | Every key-path form the validator emits maps to its form, field, and entry, and issues of other entities go to the summary list. The D4 field-state rule holds for each control kind: a field posted with its rendered value keeps its key set or unset, also when another writer changed that key; a changed field sets it; an emptied text field, list, or cleared checkbox unsets it. A re-render from posted values carries the rendered values unchanged, and a list posted with CRLF line ends or commas that parses to its rendered list counts as untouched. A new entity writes its untouched prefilled fields. | T0 `TestIssueFieldMap`, `TestFieldState` |
| W6 | Review of a valid edit answers `200` with the change summary and a diff whose hunks rebuild the candidate from the file; the file and `.bak` are unchanged afterwards. An invalid edit answers `422` with each error inside its field's element, and the file is unchanged. A non-integer port answers `422` without running the validator. A `text` review shows the plain-text diff and the one-line summary. | T1 `TestAdminDraftReview`; T0 `TestTextDiff` |
| W7 | Apply writes the reviewed text (mode 0600, `.bak` holding the previous content), reloads, and answers `303` to `return`; the next page shows the toast with the reload result; an open tunnel survives; a `proxy.listen` change shows restart required in the review, the toast, and the banner, and after that save a review that changes `proxy.listen` back to its start value lists no restart key. A reviewed `text` posted with CRLF line ends is written with LF. A `return` outside the page list gets `400`. | T1 `TestAdminApply` |
| W8 | Concurrency: each row of the D7 table, including `409` at apply after the file changed between review and apply, which leaves the file unchanged. | T1 `TestAdminDraftConflict` |
| W9 | A file with hand comments gives a review whose summary counts the removed comment lines; after apply the file equals `Encode(DocOf(file))` with the edit. | T1 `TestAdminDraftReview` |
| W10 | With an invalid file on disk, form pages are read-only with the errors banner, and every draft intent of a form op answers `409` and leaves the file unchanged. The same invalid file is fixed from the UI in three ways, each leaving the file valid, reloaded, and the banner gone: Advanced → review → apply with valid text; restore of a valid `.bak` → review → apply; and `POST /settings` with valid text, as A9. A file that differs from the running configuration shows its banner, and `POST /config/reload` reloads it. | T1 `TestAdminInvalidFile`, `TestAdminReloadButton` |
| W11 | Every new `POST` without a session, without the CSRF token, or with `Sec-Fetch-Site: cross-site` gets `403`. The test enumerates the routes registered on the mux, so a new `POST` without the checks fails it. | T1 `TestAdminPostProtection` |
| W12 | Advanced: the A9 behavior still holds; "Review changes" gives the review; `/settings/backup` shows the diff with the current file; restore goes through review and apply and leaves the replaced file in `.bak`; a `.bak` that is a link (refused by `O_NOFOLLOW`), group-writable, owned by another user, or over 256 KiB, as `Fstat` of the open descriptor reports, is not offered; a `text` or `restore` review after the file changed shows the replace warning. | T1 `TestAdminSettings`, `TestAdminBackup` |
| W13 | Shell and markup: every session page has one `h1`, a skip link, `aria-current` on its navigation entry, and a label for every input, select, and textarea; the script is the only `<script>`, has a `src`, and the A7 headers are unchanged; text and UI tokens in `admin.css` meet the D11 contrast ratios in both appearances. | T1 `TestAdminMarkup`, `TestAdminHeaders`; T0 `TestContrastTokens` |
| W14 | A directory sync that fails after the rename is reported as saved with the warning, and the reload runs (OI-0006 U-R1). | T0 `TestAtomicWriteDirSync`; T1 `TestAdminApply` |

### M4.2 — Rules and upstreams

| ID | Criterion | Check |
| --- | --- | --- |
| W15 | The rules page lists every rule in order with its action badge, patterns, ports, flags, warning badges, and 24-hour requests and bytes from `RuleTotals`, followed by the default row; `?q=` filters by name, note, pattern, and action; the empty state appears with no rules. | T1 `TestAdminRulesList`; T0 `TestStatsRuleTotals` |
| W16 | Adding (at each position), editing, duplicating, deleting, and reordering rules through the forms, with and without the position-field fallback, produce the expected candidate text; a rename or delete started at `/rules/{old}` returns to `/rules` after apply. | T1 `TestAdminRuleEdits` |
| W17 | Every `[[rule]]` and `[[upstream]]` key, `note` included, has exactly one form field on the page D4 names. | T0 `TestFormCoverage` |
| W18 | Test a URL: for a table of targets (`host:port`, `http://`, `https://` with and without a port, a bare host, IPv4 and IPv6 literals, a remote client on a closed port, remote `direct` without `allow_direct`, a rule interface list, and the literal-address cases `http://169.254.169.254/`, `127.0.0.1:8080` under a direct rule without `allow_loopback`, and a remote client with `allow_direct` to `10.0.0.1:443`), `config.Explain` gives the fields that `cuttle explain` prints, the three literal cases read "refused" with their class, and the rules page and the editor's test (against a draft that an earlier rule shadows) render them. | T0 `TestExplain`; T1 `TestAdminTestURL`; the existing `cuttle explain` tests pass with the added `address` line |
| W19 | Upstreams: the list shows the live state, usage, and traffic; add, edit, and delete work; a rename rewrites the actions and defaults that use it and returns to `/upstreams`; a used upstream has no delete, and its delete op is refused; a new upstream has no disclosure preselected, and a review without one shows the validator's message under the field. | T1 `TestAdminUpstreams` |

### M4.3 — Interfaces, proxy, remote clients, general settings, account

| ID | Criterion | Check |
| --- | --- | --- |
| W20 | With an injected interface table, the interfaces page lists the detected rows with their matches and the definitions with their states and users; "Define…" prefills the D3.6 identity (device and kind for Wi-Fi, the hardware address otherwise); a rename updates every list; a used definition has no delete; saving an untouched Define… form for a Wi-Fi row writes `device` and `kind`; no log line written by these flows contains a MAC. | T1 `TestAdminInterfaces` |
| W21 | The interface list editor adds, removes, and reorders entries for `proxy.interfaces`, a rule, and an upstream, with and without the script's controls, and "default list" and "system routing" unset the key. | T1 `TestAdminInterfaceLists` |
| W22 | Proxy, remote-client, and general forms save each of their keys; clearing a text field removes the key and the default applies; restart-only keys carry the badge. A file with `proxy.remote.ports = []` renders "No ports", and one with `admin.listen = ""` renders "Disabled"; saving their pages with another field changed keeps `ports = []` and `admin.listen = ""`. Choosing "Default" removes either key, "No ports" writes `ports = []`, "Disabled" writes `admin.listen = ""` and gives the review warning, and "These ports" or "On at" without a value gets the D4 form error. An unset `proxy.remote.default`, `allow_direct`, and `log.level` stay unset after a save of their pages that changes another field. Choosing "No ports", and separately removing one of two sources, then Back to editing, changing another field, and reviewing gives a candidate that still has `ports = []`, or lacks the removed source; the same holds through `check`, a `422` review, and Review again after a `409`. A `ports = []` that another writer saved while the Remote clients form was open survives a review in which the ports field was untouched. `TestFormCoverage` now covers every key of the 0004 D3 schema. | T1 `TestAdminSettingsForms`; T0 `TestFormCoverage` |
| W23 | Password change: a wrong current password gets `401` and counts toward the shared backoff; during a wait `429` with `Retry-After` and no Argon2id run; a short or unconfirmed new password gets `422` without hashing; success writes a 0600 Argon2id file, resets the shared failure count, ends every other session, keeps this browser signed in with a new session ID and CSRF token, and logs no password. "Sign out other sessions" ends the others only. | T1 `TestAdminPasswordChange` |

### M4.4 — Dashboard integration and browser check

| ID | Criterion | Check |
| --- | --- | --- |
| W24 | The overview shows the route sentence and stacked bar from the route totals, and the configuration summary; with statistics unavailable it shows the reason instead. | T1 `TestAdminPages` |
| W25 | "Route…" on the overview, domains, and live pages opens the prefilled rule form with the D3.12 match choices (host, each parent down to two labels, `/32` or `/128` for literals), the exact host preselected for `cdn.video.example.com` and for `user.github.io`, and the current decision; a host that is not a valid name fills nothing; "Add to rule" lists only rules with the same action and no ports; `/domains?rule=` filters by rule. | T1 `TestAdminRouteHost` |
| W26 | Headless Chrome, as in A10, with the script: log in; add a rule by pasting three domains into the list input; see the live check flag an invalid entry; see the live test shadowed by an earlier rule; reorder two rules by keyboard and review once; apply and see the toast; add an upstream; define an interface from the detected table; change the password and stay signed in; route a host from the domains page in four activations. Every page renders at 390 px wide without horizontal scrolling, in light and dark appearance, with no console error and no CSP violation report. No window opens. | T2 `scripts/admin-check` runs `TestAdminChrome` |

M4 closes with W1–W26 on one candidate tree, the T0 and T1 gates, the T2
check W26, and an independent review.

## Implementation split

One producer per task; each task opens its own `TASK-NNNN`.

| Task | Stage | Scope | Criteria | Gates |
| --- | --- | --- | --- | --- |
| A | M4.1 | `config.Doc`, writer, ops, summary, `note`, U-R1 | W1–W4, W14 | T0 |
| B1 | M4.1 | Save pipeline: draft and apply, the field-state rule and issue mapping, `internal/textdiff`, review, flash, banners, `Backend.StartConfig()`, Advanced with backup restore | W5–W12 | T0, T1 |
| B2 | M4.1 | Shell and visual system (`admin.css`, layout, icons), `admin.js` core (fragments, dialogs, toasts, unsaved guard, list input) | W13 | T0, T1 |
| C | M4.2 | Rules and upstreams pages, `config.Explain`, Test a URL, reordering, `RuleTotals`, `Filter.Rule` | W15–W19 | T0, T1 |
| D | M4.3 | Interfaces with the detected table, the interface list editor, Proxy, Remote clients, General, Account | W20–W23 | T0, T1 |
| E | M4.4 | Overview changes, Route a host, domains rule filter, the extended `TestAdminChrome` | W24–W26 | T0, T1, T2 |

A and B2 can start at once: B2 needs no part of A. B1 depends on A, for
the `Doc`, and on B2, whose components its review and banners use. C and
D depend on B1 and B2 and touch disjoint pages; they can run in parallel
in separate worktrees, with `templates/layout.html`, `admin.css`, and
`admin.js` owned by B2 and changed in C or D only by adding
page-specific blocks. E depends on C and D, because W26 also defines an
interface and changes the password. The tester runs W26 at milestone
close.

## Changes to other records

| Record | Change |
| --- | --- |
| [0004 D1][d4-1] | "cuttle never encodes TOML" becomes: the admin forms write the canonical form of this record's D6; the text editor still writes the operator's text as written. |
| [0004 D3][d4-3] | Adds the optional `note` key to `[[rule]]`, `[[upstream]]`, and `[[interface]]` (D6); binaries before it refuse a file with notes. |
| [0004 alternatives][d4-alt], [0005 alternatives][d5-alt] | The rejected "forms that rewrite the file" is now chosen by the user (2026-11-26); comments are replaced by generated ones and notes. |
| [0005 D5][d5-5] | "need no JavaScript" becomes: one same-origin script enhances the pages, which work without it (D10). |
| [0005 D6][d5-6] | `cuttle admin passwd` remains the only way to set the first password; a signed-in operator can change it with the current password (D12). |
| [0005 D9][d5-9] | The page table gains the D7 routes; "there is no JSON API" still holds, since fragments are HTML; `/domains` gains the `rule` filter. |
| [0005 D10][d5-10] | `/settings` gains Review changes and the previous version; `POST /settings` shares its save function with `POST /config/apply`. |
| [0006 D3][d6-3] | `cuttle explain` uses `config.Explain` and gains the `address` line of D8 on direct routes: the verdict for an IP-literal target, and for a name the note that its addresses are checked after resolution; its other lines do not change. |
| [0007 D7][d7-7] | `scripts/admin-check` covers W26. |
| [0008 D6][d8-6], [D15][d8-15] | MACs and interface-table addresses also appear on the admin interfaces page and in the configuration views (editor, review, `.bak` diff), which settles the wording of [OI-0006][oi-6] U-R2; validator messages, summaries, status, logs, counters, and statistics still never show a MAC. |

The amendments are made in those records when their stage lands, by the
producer of that stage, so each record matches the code it describes.

## Alternatives considered

- **A staged change set with one "commit" for many pages.** Network
  appliances work this way, but it needs a server-side draft that a
  restart loses and that diverges from the file under hand edits.
  Per-page review keeps the file the only state.
- **A JSON API with a client-rendered application.** It needs a
  framework or a large script, a build step, a second rendering path, and
  JSON escaping rules beside `html/template`. Fragments give the same
  live behavior with one template.
- **Comment-preserving TOML editing.** It needs a concrete syntax tree
  that `github.com/BurntSushi/toml` does not expose, so cuttle would
  write and maintain its own TOML parser. The user chose canonical
  rewriting.
- **Keeping the candidate on the server between review and apply.** It
  adds per-session state; posting the reviewed text back costs nothing,
  is validated again, and the `base` check guards the file.
- **Writing every effective value.** Files would grow by every default,
  and a later change of a default would never reach them; set and unset
  keys keep the file close to what the operator chose.
- **Requiring the script.** Simpler templates, but the T1 tests would
  need a browser for every flow, and a script failure would lock the
  operator out of routing changes.
- **Masking hardware addresses on the interfaces page.** The review and
  the editor show the file with the full address anyway, so masking one
  view would protect nothing.

[rm-m4]: ../ROADMAP.md#m4--full-configuration-in-the-web-admin
[d2-6]: 0002-proxy-core-and-routing.md#d6-responses-generated-by-cuttle
[d3-1]: 0003-upstream-chaining-and-exposure.md#d1-upstreams-and-disclosure-modes
[d3-10]: 0003-upstream-chaining-and-exposure.md#d10-source-classes-and-remote-policy
[d3-11]: 0003-upstream-chaining-and-exposure.md#d11-destination-address-policy-for-direct-routes
[d4-1]: 0004-configuration-and-reload.md#d1-file-and-format
[d4-2]: 0004-configuration-and-reload.md#d2-source-of-truth
[d4-3]: 0004-configuration-and-reload.md#d3-schema
[d4-4]: 0004-configuration-and-reload.md#d4-validation
[d4-6]: 0004-configuration-and-reload.md#d6-reload
[d4-7]: 0004-configuration-and-reload.md#d7-atomic-write
[d4-alt]: 0004-configuration-and-reload.md#alternatives-considered
[d5-5]: 0005-statistics-and-admin.md#d5-admin-listener-and-host-allowlist
[d5-6]: 0005-statistics-and-admin.md#d6-password
[d5-7]: 0005-statistics-and-admin.md#d7-sessions
[d5-8]: 0005-statistics-and-admin.md#d8-csrf-and-login-limits
[d5-9]: 0005-statistics-and-admin.md#d9-dashboard
[d5-10]: 0005-statistics-and-admin.md#d10-settings
[d5-alt]: 0005-statistics-and-admin.md#alternatives-considered
[d6-3]: 0006-packaging-paths-and-lifecycle.md#d3-command-line
[d7-7]: 0007-test-environment-and-gates.md#d7-gates
[d8-2]: 0008-interface-selection.md#d2-interface-definitions
[d8-4]: 0008-interface-selection.md#d4-online-state
[d8-6]: 0008-interface-selection.md#d6-interface-lists
[d8-15]: 0008-interface-selection.md#d15-status-command-line-logging-counters-and-statistics
[oi-6]: ../OPEN_ISSUES.md#oi-0006--design-follow-ups-from-the-task-0001-final-review
