# github-core-clone-01-entry

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/Iruwae/github-core-clone-01-entry/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/Iruwae/github-core-clone-01-entry/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A GitHub-compatible git forge as a single static Go binary, standard library only: git smart HTTP
(protocol v2 advertisements, ls-refs, fetch with shallow/deepen, receive-pack pushes with
per-ref report-status), REST v3 (repositories, refs, branches, commits, contents, labels,
issues, comments, pull requests, files and squash merges), GraphQL v4 (the documents `gh`
and `github-mcp-server` send, including Relay global ids and cursor pagination) and an MCP
endpoint (server/discover plus the legacy initialize handshake, with the six tools the
suite requires). Everything is in-memory and deterministic: no wall clock, no randomness in
any response, so a live trial and its replay answer byte-for-byte the same.

`main.ts` is a thin launcher: it starts the prebuilt `forge` binary beside it, reads the port
the binary prints, and runs the environment lifecycle (register, wait for done). The grader
runs `node /bot/main.ts` on Node 22; Go is not installed in the trial, so the binary is built
here and committed.

## Run it

```sh
node --experimental-strip-types main.ts
```

The platform runs `node /bot/main.ts` on Node 22.18 or later, which strips TypeScript types on
its own; the flag is harmless there and makes an older local Node work too. There is nothing to
install at run time: `main.ts` only spawns `./forge`, and the binary is statically linked.

To rebuild the binary (Go 1.27):

```sh
export PATH=$HOME/.local/go/bin:$PATH
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o forge .
```

## The lifecycle (main.ts)

The env is a server the harness starts beside yours. Its origin is in `MAKEFUN_ENV_URL`
(`http://store:8080` in a trial).

1. **Start the forge.** `main.ts` spawns `./forge`; the binary binds an ephemeral port and
   prints `[entry] listening on port <N>`. Never bind 8080: that is the env's port, and on the
   one-pod layout it is on the same loopback.
2. **Register.** `POST $MAKEFUN_ENV_URL/register` with `{"port": <the port you bound>}`, as JSON.
   The env records the address the request came from plus that port as your base URL and dials it
   from then on. The first register wins: a later one is answered `409` and does not move the
   base URL.
3. **Wait for done.** `GET $MAKEFUN_ENV_URL/done` answers at once with `{"done":false}` while the
   suite runs and `{"done":true}` once its export is written. Poll it with a short backoff; never
   hold a request open, and never exit before it says `true`.
4. **Exit 0** when it does. The launcher stops the binary first; the harness stops the env
   afterwards, and the env's export is what the scorer reads.

A failed poll or register is retried with a short exponential backoff. If `MAKEFUN_ENV_URL` is
unset there is nothing to talk to, so `main.ts` exits 1 at once.

## The surface the contest scores

| Route | What it is |
| --- | --- |
| `GET /healthz` | 200 `{"ok":true}`: the env's boot probe, called twice |
| `GET /api/v3/` | 200, `X-Oauth-Scopes: repo, read:org`, a JSON API root |
| `/api/v3/...` | GitHub REST v3: repositories, branches, commits, contents, issues, comments, labels, pull requests, files, merges |
| `POST /api/graphql` | GitHub GraphQL v4: the documents `gh` and `github-mcp-server` send |
| `POST /mcp` | Model Context Protocol over streamable HTTP: initialize, tools/list, tools/call |
| `/<owner>/<repo>.git/...` | git smart HTTP: `info/refs`, `git-upload-pack`, `git-receive-pack` |
| anything else | 404 `{"message":"Not Found"}` |

The env presents every API call with `Authorization: Bearer $MAKEFUN_SHOP_TOKEN` (the same token
is in your environment); `/healthz` is the boot probe and takes none.

The score is `feature + agent + perf` (0..1000): the conformance checks over that surface, the
MCP checks again as the agent-native column, and four timed workloads. The version of the suite
the trial runs is in the kit under `program/`, so you can grade the same checks locally before
you submit.

## Rules and limits

The rules, the scoring weights, the deadline and the prizes are on the contest page:
**https://make.fun/bounty/github-core-clone-01** (its Rules section). In short: Node 22 only, no
dependencies at run time, no internet and no `git` binary in the trial; your repository is
mounted read-only at `/bot` and you may write under `/tmp` only. The trial's CPU, memory and
process limits are the rules'; its wall budget is 900 seconds.

## Layout

- `main.ts`: the launcher and the lifecycle above. Keep it as it is.
- `forge`: the prebuilt static Go binary (linux/amd64, `CGO_ENABLED=0`).
- `main.go`: the entry point — bind an ephemeral port, serve until stopped.
- `server.go`: routing, auth and the request-id header.
- `scenario.go`: the fixed scenario identity (the viewer), derived from the contest seed.
- `git.go`: pkt-lines, pack v2 read/write, commit/tree parsing, fetch object selection.
- `store.go`: the in-memory forge — repos, git objects, contents commits, issues, pulls.
- `githttp.go`: git smart HTTP. `rest.go` + `rest_router.go`: REST v3. `graphql.go`: GraphQL v4.
  `mcp.go`: the MCP endpoint.
- `go.mod`, `package.json`, `tsconfig.json`, `LICENSE` (MIT): the entry's metadata and licence.

---

<sub>Entered on [make.fun](https://make.fun/?ref=817k0hr2&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
