# E29 — Native runtime: Hindsight 1.1.0 on pg0 under a systemd user unit

Date: 2026-10-26. Decision 0070 B's gate: is pg0 safe enough to hold the memory? `hindsight-api-slim[local-onnx,embedded-db]==1.1.0` (pg0-embedded 0.15.2, PostgreSQL 18.1 + pgvector 0.8.5) in a uv venv under `~/.cache/backsight-dev/e29`, run by the throwaway unit `backsight-e29-native.service` (API 127.0.0.1:28550, pg0 28551: `slot_port 28550`, nothing listening there). Compared with a throwaway Docker stack rendered by backsight 741df4b (`server render`, Compose project `bs-e29`, API 28600, mock LLM, the pinned `hindsight:1.1.0@sha256:50311d02…` and `pgvector:pg18@sha256:2358fcba…`). No LLM key, no memory data: the backsight docs only. **V** = observed here, **I** = inferred.

## Questions
1. What does the install cost (time, disk, Python, first-start downloads)?
2. How fast does the unit start, restart and stop?
3. Do pg0's own tools back up and restore, and does a dump cross between Docker and native (0070 C)?
4. What happens after a crash or a failed start: orphaned postgres, stale `postmaster.pid`, corruption, a lost data directory?
5. Does recall rank the same on both runtimes (E2 method)?
6. What is pg0's upstream status, and what did Hindsight issue #675 lead to?

## Method
Scripts in `E29-scripts/` (all paths through `$W`, default `$HOME/.cache/backsight-dev/e29`; `lib.sh` holds the shared helpers):
- `install.sh`: uv-managed Python 3.11 and a private `UV_CACHE_DIR` under `$W`, cold (empty cache) and warm installs, unpinned and locked to the image's own package versions (constraints read from the image's site-packages with a `docker run --rm`).
- `unit.sh <port>`: env file and unit (`HOME=$W/home`, so pg0's `~/.pg0` lands under `$W`; `HF_HOME=$W/hf`), first start, 3 stop + start, 3 restarts, each timed to `/health` = `database: connected`.
- `native-backup-restore.sh`, `cross-runtime.sh`: dump, wipe, restore; compare the row counts of all 25 public tables and the ranked top 10 of 3 probe recalls (`PROBES` in `lib.sh`).
- `crash.sh <scenario>`, `orphan.sh <scenario>`, `reinit.sh`: SIGKILL at idle and 5 s into a synchronous 200-item retain (`crash-load.py`, ~12.4 s uninterrupted), failed starts, a lost data directory.
- `bs-sandbox.sh`, `search.sh`, `score.py`: a backsight binary built from 741df4b with a sandboxed `BACKSIGHT_HOME` and `HOME` indexes the docs (`guide index`: the docs bank in chunks mode, observations, graph, temporal and keyword retrieval off, 0 LLM calls) on each runtime, then asks E31's 21 questions 5 times with `guide search --mode semantic --limit 10 --json`, plus the same recall straight on the API for latency. Results in `results-2026-10-26.jsonl`.
- `parity.sh <backsight checkout>`: chains the parity steps (build, both stacks, three runs, score). The steps were run one by one, and `install.sh` and `unit.sh` were re-run as written. `parity.sh` itself was not run end to end.
- `cleanup.sh`: unit, processes, the `bs-e29` project with its volumes, large and secret files.

Reranker: the slim install has no torch, so the image's `RERANKER_PROVIDER=local` cannot run natively; native uses `onnx` with the same model (`cross-encoder/mmarco-mMiniLMv2-L12-H384-v1`, whose repo ships `onnx/model.onnx`) and the same limits (30 candidates, 128 tokens). Embeddings are `onnx` `intfloat/multilingual-e5-small` on both. Docker was also run with the `onnx` reranker (`compose.onnx.yaml`) to separate the runtime from the reranker implementation.

## Results

### 1. Install (**V**)
| Measure | Value |
|---|---|
| Python | `requires_python >=3.11`; the image runs 3.11.17, uv installed 3.11.15 (95-100 MB, 1.0-4.3 s cold) |
| Unpinned install (today's resolve) | cold 7.2-8.4 s, warm 0.45-0.49 s (3 runs); 186 packages, **128 at another version than the image** (e.g. fastmcp 3.2.0 → 4.1.4, mcp 1.30.0 → 2.3.0, anthropic 0.99.0 → 2.3.0, cohere 5.20.1 → 7.3.0, transformers 5.12.1 → 5.15.1); it still starts, retains and recalls (smoke) |
| Locked install (`-c` the image's versions) | cold 6.3-7.0 s, warm 0.41-0.45 s; 0 versions differ from the image |
| Disk | venv 917 MB (apparent; `du` 1.0 GB), uv cache 1.1 GB (cold only), pg0 binary 32.6 MB inside the wheel, Postgres extracted to `~/.pg0/installation/18.1.0` 74 MB, instance 43 MB empty schema, 84 MB with the test data, models 941 MB (e5-small 470 MB + mmarco 471 MB) |
| Downloaded at first start | only the two HF models. PostgreSQL 18.1, pgvector 0.8.5 and their runtime libs are `include_bytes!` in the pg0 binary and extracted, no download (pg0 v0.15.2 `src/main.rs` l.15-24) |
| Memory | hindsight-api peak RSS 2.37-2.45 GB (Docker container 2.17 GiB + db 38 MiB) |

### 2. Start, restart, stop under the unit (**V**)
| Measure | Native (unit) | Docker (`bs-e29`) |
|---|---|---|
| First start, empty model cache and no instance | **26.8 s** (941 MB of models downloaded; pg0 extract + initdb 1.2 s) | 40.7 s (images cached, empty volumes) |
| Warm start to healthy | **7.7-8.1 s** (6 runs) | 22.9-25.7 s (`compose stop`/`up -d`, 3 runs: db healthcheck + start-all) |
| Restart | 7.9-8.4 s (6 runs) | — |
| Stop | 0.21-0.29 s, clean: Postgres logs "database system is shut down", the next start "was shut down at" | 1.4-2.3 s |

Warm start breakdown (log): imports 2.3 s, embeddings 2.5 s, pg0 start 0.27 s, reranker 2.4 s, migrations 0.06 s. Under the unit the postgres processes sit in the unit's cgroup; on stop, systemd's SIGTERM and Hindsight's own `pg0 stop` both shut it down cleanly.

### 3. Backup and restore (**V**)
| Step | Result |
|---|---|
| pg0's tools | `pg_dump`, `pg_restore`, `psql`, `pg_amcheck` … 18.1 under `~/.pg0/installation/18.1.0/bin`; they need `LD_LIBRARY_PATH=<that>/lib` (bundled libicu 70), else `libicuuc.so.70: cannot open shared object file` |
| Online `pg_dump -Fc` (API running) | 84-95 ms, 551 KB (91 docs, 94 chunks, 3 362 links); `pg_restore --list` lists vector, pg_trgm, btree_gin |
| Wipe and restore | unit stopped, `pg0 drop` (data dir deleted), `pg0 start` alone (fresh initdb), `dropdb`/`createdb`, `pg_restore --no-owner --exit-on-error` 87-89 ms, `pg0 stop`, unit started: 10.9 s stop to healthy |
| Check | 25 tables' row counts identical, the 3 probes' ranked top 10 identical |
| Docker → native | dump made in the db container (PostgreSQL **18.6**, pgvector **0.8.7**), restored into pg0 (PostgreSQL **18.1**, pgvector **0.8.5**): ok, the extension created at 0.8.5; counts and probes identical |
| Native → Docker | pg0's `pg_dump` 18.1, restored in the db container (hindsight service stopped): ok, extension at 0.8.7; counts, probes and a marker bank written natively identical |

Both directions work today because the dump does not pin the extension version and neither schema uses anything newer than pgvector 0.8.5 (**I**). Docker → native goes to an older minor and an older pgvector: a future Hindsight or pgvector feature could break that direction.

### 4. Crash and failed start
| Scenario | Postgres after | Next start | Data | |
|---|---|---|---|---|
| SIGKILL hindsight-api (main pid), idle | unit `failed (signal)`; systemd SIGTERMs the rest of the cgroup: postgres shut down cleanly, no pid file | healthy in 8.6 s | counts and probes unchanged | V |
| SIGKILL main pid 5 s into a 200-item synchronous retain | same | healthy in 8.5 s | client `curl: (52) Empty reply`; **83 of 200 documents committed** (chunks and units consistent per document) | V |
| `systemctl kill -s SIGKILL` the whole unit, idle | no postgres left, stale `postmaster.pid` kept | healthy in 8.1 s: pg0 removes the stale pid file, Postgres "not properly shut down; automatic recovery", redo in 0.00 s | unchanged | V |
| Whole unit SIGKILLed mid-retain | same | healthy in 8.1 s, redo 0.02 s | 82 of 200 documents committed, probes unchanged | V |
| Stale `postmaster.pid` naming a live non-postgres pid | — | healthy in 7.9 s (pg0 checks the process is postgres, fix of pg0 #37) | unchanged | V |
| Failed start under the unit (reranker model that does not exist; pg0 already up) | unit `failed`, **no postgres left** (KillMode=control-group) | healthy in 7.9 s | — | V |
| The same failed start from a shell | **orphan postgres** keeps the port | the unit's start reuses it silently (healthy, postmaster outside the unit's cgroup); **stopping the unit leaves it running** | — | V |
| hindsight-api run from a shell, SIGKILLed | orphan postgres | same as above | — | V |
| Unit with `KillMode=process`, main pid SIGKILLed | orphan postgres in the unit's cgroup | reused; survives the next stop | — | V |
| Instance directory missing | — | **healthy, 0 banks**: pg0 runs initdb, Hindsight migrates from scratch, nothing in the log | the old data intact where it was moved | V |
| Data directory present but empty (unmounted mount point) | — | same: healthy and empty, no warning | intact elsewhere | V |
| After all of the above | `pg_amcheck --heapallindexed --rootdescend`: no error; `checksum_failures` 0 (data checksums on) | | | V |

- **fsync is off by default**: pg0's dependency `postgresql_embedded` 0.20.0 starts Postgres with `-F -p <port>` (https://github.com/theseus-rs/postgresql-embedded/blob/v0.20.0/postgresql_embedded/src/postgresql.rs#L290); `show fsync` = `off`, source `command line`. A process crash loses nothing (the OS still holds the pages, see above); an OS crash or power loss can corrupt the cluster (**I**, PostgreSQL docs on `fsync`). `pg0://hindsight:<port>?fsync=on` in `HINDSIGHT_API_DATABASE_URL` is passed after `-F` and wins: `on (command line)` (**V**).
- The #675 guard is not in the pip package (0 files mention `PG_VERSION` or #675 in `hindsight_api` or `pg0`): it lives in the Docker image's `start-all.sh` only, and is a warning, not a refusal (**V**).
- pg0 listens on 127.0.0.1 and ::1 with password auth; Hindsight's default credentials are `hindsight`/`hindsight` and `~/.pg0/instances/<name>/instance.json` (mode 0644, dir 0755) stores the password in clear. Docker's database is not published on the host at all (**V**).
- Hindsight's URL has no data-dir option: pg0's data follows `$HOME` (**V**, `hindsight_api/pg0.py`), so the unit moves `HOME`.
- The mid-retain loss is Hindsight's (a synchronous retain commits per document), not pg0's: the same would happen in Docker (**I**). Resending with the same document ids completes it.

### 5. Recall parity (**V**, `results-2026-10-26.jsonl`, 21 questions x 5 runs)
| Runtime | MRR | hit@1 | hit@3 | same top 10 on all 5 runs | API recall p50 / p95 (ms) | `guide search` p50 / p95 (ms) |
|---|---|---|---|---|---|---|
| native, onnx reranker (run 1) | 0.496 | 0.33 | 0.67 | 21/21 | 614 / 724 | 631 / 737 |
| native, onnx reranker (run 2) | 0.496 | 0.33 | 0.67 | 21/21 | 580 / 701 | 601 / 714 |
| Docker, local (torch) reranker, as shipped | 0.496 | 0.33 | 0.67 | 21/21 | 524 / 624 | 549 / 638 |
| Docker, onnx reranker | 0.496 | 0.33 | 0.67 | 21/21 | 535 / 713 | 552 / 735 |

- **overlap@10 = 1.000 and the same ranked top 10 on 21/21 questions** for every pair (native vs both Docker runs, torch vs onnx reranker).
- Native recall is 8-15 % slower at p50 (cause not isolated).
- The MRR is lower than E31's 0.568 because the docs changed (90 sections at 741df4b vs 86 in PR 252); Docker gives the same 0.496 on these docs.

### 6. Upstream (checked 2026-10-26)
| What | Fact | Source |
|---|---|---|
| pg0 release | v0.15.2 (2026-09-14) is the latest, the one Hindsight 1.1.0's image ships; repo last pushed 2026-10-24; GitHub reports no licence file (MIT in Cargo.toml, C01) | https://github.com/vectorize-io/pg0/releases/tag/v0.15.2 |
| "Development" label | Hindsight docs, "Development with pg0": "Development \| pg0 (embedded) \| Automatic; Production \| PostgreSQL 15+"; same page still says port 5555 and `~/.hindsight/pg0/` (wrong, C09); last changed 2026-07-07. pg0's README: "Use pg0 for local development, testing, CI/CD pipelines…"; PyPI classifier "Development Status :: 4 - Beta" | https://github.com/vectorize-io/hindsight/blob/main/hindsight-docs/docs/developer/storage.md ; https://github.com/vectorize-io/pg0 ; https://pypi.org/project/pg0-embedded/0.15.2/ |
| Pin | Hindsight 1.1.0 asks `pg0-embedded>=0.15.2` (no upper bound) | https://pypi.org/pypi/hindsight-api-slim/1.1.0/json |
| #675 | opened 2026-03-24: "Embedded pg0 database loses all data after docker restart" (v0.4.19, Docker Desktop extension); the reporter found pg0 ran initdb on an empty data dir; closed 2026-03-27 by PR #698 (commit 6e90df9): SIGTERM trap in `start-all.sh` so pg0 shuts down cleanly, and a startup **warning** when the data dir has no `PG_VERSION` | https://github.com/vectorize-io/hindsight/issues/675 ; https://github.com/vectorize-io/hindsight/pull/698 |
| Same period in pg0 | #6 "pg0 start deletes data directory on stale postmaster.pid (data loss after unclean shutdown)", opened 2026-03-21, closed 2026-03-26; v0.15.2 removes only the pid file (`check_not_running`, verified above) | https://github.com/vectorize-io/pg0/issues/6 |
| After #675 | #1572 OOM-killed container then the #675 warning (closed 2026-06-02); #1638 false-positive warning on the nested `instances/<name>/data` layout (closed 2026-05-25) | https://github.com/vectorize-io/hindsight/issues/1572 ; https://github.com/vectorize-io/hindsight/issues/1638 |
| Other pg0 lifecycle bugs, closed | #17 stop returned before the postmaster exited (2026-05-28); #24 `info` "running" with no connectivity check, after logind `RemoveIPC` reaped the shared memory (2026-07-29); #36 start failed after a clean stop, `missing field pid` (2026-09-14); #37 start refused forever on a reused pid after reboot (2026-09-14) | https://github.com/vectorize-io/pg0/issues/17 ; https://github.com/vectorize-io/pg0/issues/24 ; https://github.com/vectorize-io/pg0/issues/36 ; https://github.com/vectorize-io/pg0/issues/37 |
| Open | pg0 #45 (2026-10-25) Python `Pg0.start()` can hang forever on Linux (postgres inherits the pipes; not seen in ~30 starts here); pg0 #41 (2026-09-27) Windows `info` says stopped for a live instance; Hindsight #6093 (2026-10-25) a daemon tries to own pg0 against a running cluster; #6065 (2026-10-24) pg0 fails on Windows zh-CN (UTF-8). No open Hindsight issue matches "data loss" or "corruption" | https://github.com/vectorize-io/pg0/issues/45 ; https://github.com/vectorize-io/pg0/issues/41 ; https://github.com/vectorize-io/hindsight/issues/6093 ; https://github.com/vectorize-io/hindsight/issues/6065 |

## Verdict for 0070
**pg0 passes, under conditions.** On this machine it ran the same data as Docker. Backups and restores were exact, both within pg0 and across runtimes, and recall ranked identically. Every process crash recovered with no loss beyond Hindsight's own non-atomic retain. Upstream fixed the destructive bugs (#6, #37, #36, #17). The risks left are pg0's defaults and its silence about lost data, which backsight's native runtime has to cover:

1. **fsync on**: `?fsync=on` in the pg0 URL always. pg0's default (`-F`) can corrupt the cluster on an OS crash or power loss.
2. **No silent empty database**: before each start, refuse when the instance backsight initialised is missing or empty. Compare a recorded identity, such as the `pg_controldata` "Database system identifier" or a marker row, and offer a restore instead. Upstream's #675 guard only warns, and only in Docker.
3. **Stale-process guard**: the unit keeps `KillMode=control-group`, and nothing starts hindsight-api outside it. Before start, restore and backup verify, a postgres on the instance's data directory that is not in the unit's cgroup is reported and stopped (`pg0 stop`), never adopted. Otherwise a shell run or a failed start leaves an orphan that the unit reuses and never stops.
4. **Pinned versions**:
   - Python 3.11, with the image's patch release.
   - A hash-locked dependency set per OS and architecture, taken from the pinned image's versions. An unpinned resolve today differs on 128 of 186 packages.
   - `pg0-embedded==0.15.2`, which Hindsight leaves open-ended.
   - Native ships PostgreSQL 18.1 and pgvector 0.8.5, while Docker runs 18.6 and 0.8.7. A runtime switch is a restore into an older minor, so `backup verify` must run in the target runtime.
5. **Daily verified backups** stay mandatory (SPEC §11.8): pg0's bundled `pg_dump` with its `lib` directory on `LD_LIBRARY_PATH`, and `backup verify` into a throwaway pg0 instance.
6. **Secrets and paths**:
   - Set a random database password in the URL (`pg0://hindsight:<secret>@hindsight:<port>`).
   - Keep `~/.pg0` (the unit's `HOME`) under backsight's 0700 data directory, because pg0 writes the password into a 0644 `instance.json`.
   - Set `HF_HOME` explicitly.
7. **User session**: keep linger on (it is here), so logind's `RemoveIPC` cannot cut the shared memory of a running pg0 (pg0 #24) (**I**).
8. **Reranker**: native uses the `onnx` provider. It ranked the same as the image's torch reranker on these 21 questions.

## Addendum, 2026-10-29: pg0 under its own unit (M7 block 1 Task 1)

Run 2026-10-30 for decision 0078 ("The database with the API stopped"), with `E29-scripts/db-unit.sh` (steps `setup c1 c2 c1pw c3 c4 c5 tools c6 c7 cleanup`). `W=~/.cache/backsight-dev/m7-db-unit`. The venv is a symlink to E29's locked venv (`hindsight-api-slim` 1.1.0, `pg0-embedded` 0.15.2): `install.sh` has no locked-only mode, so it was not re-run. Units: `bs-m7check-db.service` and `bs-m7check-server.service`, with transient `bs-m7check-maint`. API on 28650, Postgres on 28651. Mock LLM, no data. The db password is 32 hex characters from `/dev/urandom`, written by the shell (never on an argv) to `$W/db.pw` and `$W/native.env` (0600). **V** = observed, **I** = inferred.

| Check | Result | |
|---|---|---|
| **C1** `pg0 start --name hindsight --port 28651 -u hindsight -n hindsight -c fsync=on` as `ExecStart` | pg0 **does not stay in the foreground**: it exits once the postmaster is up (pg0 `src/main.rs` `std::mem::forget(postgresql)` after `pg_ctl start -w`). With `Type=simple`, the unit is `inactive dead` within 0.2 s. systemd's SIGTERM gives "received smart shutdown request" and then "database system is shut down". With `Type=forking`, `PIDFile=<data>/postmaster.pid` and `ExecStop=pg0 stop --name hindsight`, the unit is `active running`, MainPID = postmaster, and all 10 postgres processes are in the unit's cgroup. `show fsync` = `on (command line)`. The postmaster still gets pg0's `-F` with `-c fsync=on` after it, plus pg0's defaults (`shared_buffers=256MB`, `work_mem=64MB`, `maintenance_work_mem=512MB`, `effective_cache_size=1GB`, `max_parallel_maintenance_workers=4`, `timezone=UTC`, `log_timezone=UTC`, `logging_collector=on`). Stop takes 127 ms: "received fast shutdown request", "database system is shut down", no postgres left. | V |
| C1, password | The only way to give pg0 a password is `-P` (argv). There is no environment variable: pg0's CLI has `-P`/`--password` only. **Every** `pg0 start` runs `psql postgresql://postgres:<password>@127.0.0.1:<port>/postgres -c "DO … CREATE USER …"`, so the password is on that psql's argv: the 50 ms sampler caught it once with the default `postgres`. pg0 prints `Password:` and the connection URI to stdout, which lands in the unit's log. With the roles' passwords changed through SQL and no `-P`, `pg0 start` fails: `password authentication failed for user "postgres"`. The unit goes `failed` with no postgres left, and pg0 has already **deleted `instance.json`** (`remove_instance` before `start`), so `pg0 stop --name hindsight` would then answer "no instance". | V |
| **C2** pg0's extracted `postgres` as a `Type=simple` `ExecStart` | **Instance creation**: in `systemd-run --user --wait --collect --pipe --unit bs-m7check-maint -p KillMode=control-group -E HOME=<home> -E PGPASSWORD=postgres`, the transient unit runs `pg0 start … -c fsync=on` with pg0's default password, then `psql -h 127.0.0.1 -U postgres -d postgres -v ON_ERROR_STOP=1 -f -`, which reads `ALTER ROLE hindsight PASSWORD '…'; ALTER ROLE postgres PASSWORD '…';` from stdin (printed by the shell's `printf` into `systemd-run --pipe`), then `pg0 stop --name hindsight`. Everything ran in the maintenance unit's cgroup; it exited 0 with `postmaster.pid` gone. **The unit**: `ExecStart=<inst>/bin/postgres -D <data> -p 28651 -c fsync=on -c listen_addresses=127.0.0.1 -k <home>/.pg0/run`, `Environment=LD_LIBRARY_PATH=<inst>/lib`, `KillMode=mixed`, `KillSignal=SIGINT`, plus a readiness `ExecStartPost` (below). It is ready in 133-135 ms; MainPID = postmaster, and all 9 processes, later 14 with Hindsight's connections, are in the db unit's cgroup. `show fsync` = `on (command line)`. The default password is refused. Stop takes 21-23 ms: "received fast shutdown request", "database system is shut down". | V |
| C2, missing / empty data directory | Both **fail**, and nothing is initialised. Missing: `could not access directory "…/data": No such file or directory` and "Run initdb or pg_basebackup…" (exit 2), unit `failed`, directory still absent. Empty: `could not access the server configuration file "…/data/postgresql.conf"`, unit `failed`, directory still empty (0 entries, no `PG_VERSION`). With the first readiness loop (`for i in $(seq 1 300) … pg_isready`) each failure took 31 s, because the loop polled after the postmaster died. With `ExecStartPost=/bin/sh -c 'while kill -0 $$MAINPID 2>/dev/null; do <inst>/bin/pg_isready -q -h <sock> -p 28651 && exit 0; sleep 0.1; done; exit 1'` each failed in 0.1 s. | V |
| C2, differences from pg0's own start | Without pg0's `-c` set, the cluster runs `postgresql.conf` defaults: `shared_buffers` 128MB, initdb's system time zone (CET in the logs), `logging_collector=off` (the log goes to the unit's stderr), and it listens on 127.0.0.1 only (pg0: 127.0.0.1 and ::1, socket in `/tmp`). Hindsight was healthy on it. Passing pg0's `-c` defaults on `ExecStart` keeps parity with what Hindsight gets under pg0 (**I**). `-k <home>/.pg0/run` is not created by pg0, so the script makes it (0700) before the start. | V / I |
| **C3** `bs-m7check-server.service` (`Requires=`/`After=` the db unit, `EnvironmentFile`, `HINDSIGHT_API_DATABASE_URL=postgresql://hindsight:<pw>@127.0.0.1:28651/hindsight`) | `systemctl --user start bs-m7check-server.service` pulled in the db unit. **Healthy in 24.7 s** on the first start, models downloaded; `{"status":"healthy","database":"connected",…}`. 25 public tables; `vector` 0.8.5, created by Hindsight's migrations, since pg0 makes `hindsight` a superuser. No postgres in the server's cgroup. `systemctl --user stop bs-m7check-db.service` took 529 ms and **left both units `inactive dead`**, with "database system is shut down" and no error in the server log. | V |
| **C4** no password on any argv | `ps -eo pid,args` was sampled every 50 ms through C1, C2 (creation, start, stop, missing/empty), C1 with the changed password, C3, and C4: the server up plus `pg_dump -Fc` (83 807 bytes, 25 tables) with `PGPASSWORD` in the environment. Samples: 73, 1 003 + 62, 25, 395, 135 + 136. **0 lines** hold the password. The secret is also absent from the unit logs, Postgres' logs and `instance.json`. The short `psql -f -` of the creation was not caught by the sampler: that it reads the secret only from stdin is by construction (**I**). | V |
| C4, where pg0 keeps a password | `instance.json` (0644) keeps the password pg0 was **started** with: after the C2 creation that is pg0's default `postgres`, which no longer works. pg0's dependency `postgresql_embedded` 0.20.0 makes `tempfile::tempdir().into_path()/.pgpass` (dir 0755, file 0644) for initdb's `--pwfile` and **never removes it**. Each instance creation left `/tmp/.tmpXXXXXX/.pgpass` with the creation password, and each `pg0 start` left one or two more `/tmp/.tmp*` directories, all of them deleted here. This machine had 80 such directories from earlier pg0 runs (left alone). | V |
| **C5** models | First start (HF_HOME=`$W/hf`, 941 MB): `intfloat/multilingual-e5-small` @ `614241f622f53c4eeff9890bdc4f31cfecc418b3` and `cross-encoder/mmarco-mMiniLMv2-L12-H384-v1` @ `1427fd652930e4ba29e8149678df786c240d8825`, each `config.json onnx/model.onnx sentencepiece.bpe.model special_tokens_map.json tokenizer_config.json tokenizer.json`. `<venv>/bin/hf download <repo> --revision <sha> --include <each file>` into an empty `HF_HOME` gives the same files. A start with `HF_HUB_OFFLINE=1` on that cache **failed** (`LocalEntryNotFoundError: Cannot find an appropriate cached snapshot folder for the specified revision`): Hindsight asks for `main`, and a download by commit writes no `refs/main`. After `printf '%s' <sha> > $HF_HOME/hub/models--<org>--<name>/refs/main` the offline start was **healthy in 6.4 s**. The override has to be a later `EnvironmentFile`: a drop-in `Environment=HF_HOME=…` loses to the unit's `EnvironmentFile` (first attempt, which silently used the old cache). | V |
| **C6** uv | Releases (GitHub API): 0.13.6 (10-27), 0.13.5 (10-22), 0.13.4, 0.13.3 (10-20), 0.13.2 (10-19), **0.13.1 (2026-10-14T05:15:30Z)**, 0.13.0 (10-09). The newest at least 14 days old on 2026-10-29 is **0.13.1**. `uv-x86_64-unknown-linux-gnu.tar.gz` sha256 `eadd9d27c134ab5ac1ffd4473b97a741407f3ea238288f03121fc4deddf90220`, from the release's `.sha256` asset and matching the computed hash. `uv 0.13.1 python list --only-downloads 3.11`: **`cpython-3.11.17-linux-x86_64-gnu`**, the image's own patch (E29's uv 0.11.7 installed 3.11.15). | V |
| **C7** `systemd-run --user --wait --collect --unit bs-m7check-maint -p KillMode=control-group <cmd>` | The main process and a `setsid` child are both in `…/app.slice/bs-m7check-maint.service`; when the main process ended, the child was **gone** and the unit `inactive`. `pg0 start` run as such a unit's command (the fallback's maintenance unit as written) exited 0 with the postmaster in the transient cgroup; the unit then ended at once, and systemd shut Postgres down ("received smart shutdown request"). A maintenance unit that must keep Postgres up has to stay alive itself: `Type=forking`/`PIDFile`, `RemainAfterExit`, or a foreground postgres. systemd expands `$` in a `systemd-run` command line, so a literal `$` needs `$$`. | V |
| Tools for later tasks | `<home>/.pg0/installation/18.1.0/bin` has `pg_controldata`, `pg_dump`, `pg_restore`, `psql`, `pg_isready`, `pg_ctl` and `initdb`, all 18.1. `LD_LIBRARY_PATH=<inst>/lib pg_controldata <data>` prints `Database system identifier: 7702339718603615110` and `Database cluster state: shut down` (it also runs without `LD_LIBRARY_PATH`). | V |
| Cleanup | `db-unit.sh cleanup`: both units stopped, unit files removed, `daemon-reload`, `reset-failed`. `systemctl --user list-units 'bs-m7check-*' --all` lists 0 units; 0 user postgres processes; nothing on 28650 or 28651. Instance, models, env file and password deleted. Kept: `$W/results` (ps samples gzipped, outputs) and `$W/logs`. | V |

**Verdict: preferred-postgres** (C2). 0078's two units stay, with `postgresql://`.
- **db unit**:
  - `Type=simple`
  - `ExecStart=<home>/.pg0/installation/18.1.0/bin/postgres -D <home>/.pg0/instances/hindsight/data -p <pg port> -c fsync=on -c listen_addresses=127.0.0.1 -k <home>/.pg0/run` with `Environment=LD_LIBRARY_PATH=<installation>/lib`. pg0's other `-c` defaults are worth adding (**I**).
  - No `ExecStop`: `KillMode=mixed` and `KillSignal=SIGINT` give a fast shutdown.
  - `ExecStartPost=/bin/sh -c 'while kill -0 $$MAINPID 2>/dev/null; do <inst>/bin/pg_isready -q -h <sock> -p <pg port> && exit 0; sleep 0.1; done; exit 1'`.
  - The socket directory is created before the start.
  - A missing or empty data directory fails the start and never runs initdb.
- **Instance creation, with no password on any argv**:
  - Run once, in `systemd-run --user --wait --collect --pipe -p KillMode=control-group -E HOME=<home> -E PGPASSWORD=postgres`: `pg0 start --name hindsight --port <pg port> -u hindsight -n hindsight -c fsync=on`, then `psql -h 127.0.0.1 -U postgres -d postgres -v ON_ERROR_STOP=1 -f -` with `ALTER ROLE hindsight PASSWORD '<pw>'; ALTER ROLE postgres PASSWORD '<pw>';` on stdin, then `pg0 stop --name hindsight`.
  - pg0 has no password environment variable. `instance.json` keeps pg0's default `postgres`, and a `/tmp/.tmp*/.pgpass` holding it is left behind, to delete.
- **C5 fetch**: for each model, `HF_HOME=<hf> <venv>/bin/hf download <repo> --revision <sha> --include config.json --include onnx/model.onnx --include sentencepiece.bpe.model --include special_tokens_map.json --include tokenizer_config.json --include tokenizer.json`, then `printf '%s' <sha> > <hf>/hub/models--<org>--<name>/refs/main`. Pins:
  - `intfloat/multilingual-e5-small` @ `614241f622f53c4eeff9890bdc4f31cfecc418b3`
  - `cross-encoder/mmarco-mMiniLMv2-L12-H384-v1` @ `1427fd652930e4ba29e8149678df786c240d8825`
- **C6**: uv **0.13.1**, `uv-x86_64-unknown-linux-gnu.tar.gz` sha256 `eadd9d27c134ab5ac1ffd4473b97a741407f3ea238288f03121fc4deddf90220`, Python **3.11.17**.

Consequences for 0078 and the plan:
1. The image's patch, 3.11.17, is available, so "the closest 3.11" does not apply.
2. With pg0 used only to create the instance, pg0's password, `instance.json` and the `/tmp` `.pgpass` matter only at creation.
3. A failed `pg0 start` deletes `instance.json`, so the stale-process guard should not depend on `pg0 stop`. `pg_ctl stop -D <data> -m fast` from the installation does not depend on it (**I**, not run here).
4. The fallback (`pg0://` in the single unit) would have Hindsight call `pg0 start -P <pw>`, which puts the password on argv (**I**, from the code: `pg0/__init__.py` `Pg0.start()` runs `pg0 start … --password <pw>` as a subprocess). Its maintenance unit would end, and so stop Postgres, as soon as `pg0 start` returns (C7, **V**).
