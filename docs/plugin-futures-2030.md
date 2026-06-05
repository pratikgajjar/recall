# The plugin escape hatch, four years out (mid-2030)

A decision about Tier-1 (how non-declarative plugins are written) is really a bet
on what recall's ecosystem looks like in 2030. Tier-0 declarative specs are in
every scenario — the question is the escape hatch for the long tail. Here is each
option projected forward.

Legend: `■` core/maintained-by-us · `□` community/plugin · `▒` sandbox boundary

---

## Option A — External process (NDJSON over stdio)

```
                    2030: recall = a thin, stable host
                    ────────────────────────────────────
        □ recall-amp (Go)        □ recall-chatgpt-export (Python)
                   \                   /
   □ gemini.json ── ■ recall host ── □ recall-devin (Node)
   □ windsurf.json    │  (~11MB, no  \
   □ cline.json       │   CGO, fast)  □ recall-inhouse-acme (bash, private)
                      │
              `recall plugin add user/repo`  → 300+ community plugins
```

**Scene.** recall became "the ripgrep of agent history." The core team hasn't
written an agent adapter in years — they maintain *the contract*. New agent
trends on a Tuesday; by Thursday there's a Tier-0 spec or a 40-line `recall-x`
script in someone's repo. Cloud-only agents (Amp, ChatGPT/Claude web exports,
Devin) are covered because a process can hit an API — the thing declarative
specs never could. Companies index proprietary agents privately and never tell
us they exist.

- **Ecosystem:** widest. Any language → most agents covered, including the weird
  and the remote.
- **Maintainer burden:** lowest. You own one serialized contract, versioned once.
- **What broke:** trust. Arbitrary executables are a supply-chain surface. 2030
  reality: a signed registry + a read-only sandbox convention + `plugin add`
  showing what it installs. The occasional "needs Python 3.13 installed" friction.
- **Verdict:** maximum reach, minimum core. Bets on community over control.

---

## Option B — Embedded scripting (Starlark/Risor, pure Go)

```
        2030: recall ships a script runtime inside the binary
        ──────────────────────────────────────────────────────
   □ gemini.json ──┐        ┌─ ▒ aider.star ▒
   □ cline.json  ──┤  ■ recall host  ▒ opencode.star ▒
                   │  + embedded VM  ▒ goose.star ▒
                   └─ ■ host API (the second contract you now own forever)
                            │
                    ~80 plugins · no external runtime needed · cloud = awkward
```

**Scene.** Plugins are `.star` files — more than data, safer than processes, and
they run with zero external runtime, so "works on my machine" mostly vanished.
But authors must learn the embedded language *and* recall's host API. That API
became a **second contract** you version and defend forever; a v2 cleanup in
2028 broke a wave of plugins and burned goodwill. The sandbox has no real network,
so cloud agents stayed unsupported or needed host-mediated fetch you had to build.

- **Ecosystem:** medium. More than JSON-only, fewer casual authors than a script
  they can write in their own language.
- **Maintainer burden:** medium-high. You own a language surface and its stdlib.
- **What broke:** API churn → plugin breakage; cloud sources awkward by design.
- **Verdict:** the "controlled middle." Comfortable, but you signed up to be a
  platform-language maintainer.

---

## Option C — WASM (wazero, sandboxed, pure Go)

```
        2030: plugins are signed .wasm, sandboxed and fast
        ──────────────────────────────────────────────────
   □ gemini.json ──┐     ┌ ▒▒ recall-amp.wasm  (Rust) ▒▒
   □ cline.json  ──┤ ■ recall host ▒▒ goose.wasm (TinyGo) ▒▒
                   │ + wazero +     ▒▒ ...few, but bulletproof ▒▒
                   └ ■ host ABI / WASI (you track the spec churn)
                          │
                  enterprises love it · long tail underserved
```

**Scene.** Plugins are `.wasm` — sandboxed, fast, capability-scoped. By 2030 WASI
matured, so filesystem and metered network are standardized; recall became the
*secure* choice and enterprises adopted it for exactly that. But the toolchain
barrier (compile to wasm, target the host ABI) kept casual and in-house authors
away. The catalog is smaller and skews to popular agents; the long tail and
quick-hack private agents never materialized. A couple of language SDKs softened
it, not enough.

- **Ecosystem:** narrow but high-quality and high-trust.
- **Maintainer burden:** medium-high. Host ABI + WASI version tracking.
- **What broke:** adoption breadth — "easy to write plugins" lost to "safe to run
  plugins." Binary grew with the runtime.
- **Verdict:** the enterprise/security bet. Strong moat, smaller world.

---

## Option D — Declarative only (defer any code escape hatch)

```
        2030: a beautiful, tiny, bounded core
        ──────────────────────────────────────
   □ gemini.json   ■ recall host (~10MB, fastest, 100% safe)
   □ cline.json    │
   □ windsurf.json │   ┌──────────── wall ────────────┐
   □ ...the shapes │   ╎  Aider (Markdown)  ✗          ╎
                   │   ╎  Amp / web exports ✗   long   ╎
   spec JSON slowly grows ╎  in-house oddballs ✗  tail ╎
   conditionals + transforms └──────────────────────────┘
        → "inner-platform": a bad programming language, in JSON
```

**Scene.** recall stayed jewel-like: tiny, instant, perfectly safe, no trust
surface. It covered everything that fits the four shapes — which is most agents.
But every edge case became pressure to add "just one more" spec knob, and by
2030 the JSON had grown conditionals, fallbacks, and string transforms: an
**inner-platform effect**, a half-language expressed in config. Markdown and
cloud agents never arrived; a few users forked to add Go. Lovely at its job,
quietly ceded the long tail.

- **Ecosystem:** solid for the common shapes, blind to the rest.
- **Maintainer burden:** low — but with chronic "add a feature to the spec" creep.
- **What broke:** the spec slowly reinvented programming, badly; long tail lost.
- **Verdict:** safest and smallest. Wins the 90%, abandons the 10% that includes
  tomorrow's surprises.

---

## Side by side, 2030

| | A · External proc | B · Embedded script | C · WASM | D · Declarative only |
|---|---|---|---|---|
| Agents covered | **widest** (+cloud) | broad | popular only | shapes only |
| Ease of authoring | **highest** (any lang) | medium | low (toolchain) | highest *if it fits* |
| Cloud/remote sources | ✅ | ⚠️ host-mediated | ✅ (metered) | ❌ |
| Single static binary | ✅ | ✅ (bigger) | ✅ (bigger) | ✅ (smallest) |
| Trust/security | ⚠️ arbitrary code | ✅ sandbox | ✅✅ sandbox | ✅✅ data only |
| Contracts to maintain | **1** | 2 (proto+lang API) | 2 (proto+ABI) | 1 |
| Core team writes agents? | no | rarely | rarely | for edge cases / never |
| Failure mode | supply-chain trust | API churn | small catalog | inner-platform DSL |
| Feels like | ripgrep/fzf plugins | a scripting platform | a secure runtime | a beautiful dead-end |

## The shape of the bet

- **A** maximizes reach and minimizes what *you* maintain — community-scale, at
  the cost of a trust surface you manage with signing + convention.
- **D** is the floor everyone shares; alone, it slowly turns config into code.
- **B/C** trade breadth for safety and put you in the business of maintaining a
  language/ABI for years.

The recommendation in `indexable-architecture.md` — **Tier 0 (D) as the default
+ Tier 1 (A) as the escape hatch** — is the only combination that is both *safe
for the 90%* and *unbounded for the 10%*, while leaving you exactly **one**
serialized contract to own. B and C are things you can add *later behind the same
NDJSON contract* (an embedded or wasm runner is just another way to produce the
same records) — so choosing A now doesn't foreclose them; choosing B or C now
does foreclose the long tail.
</content>
