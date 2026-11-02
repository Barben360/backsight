# Server shapes the collection reads

Field paths and JSON types only, never values. Each field the code relies on
names its source: an existing decoder of this package, the field name
assumed from the task, or the live probe (GET only, 2026-11-02, field paths
only; a type is given where the probe reported one).

All paths are under `/v1/default/banks/<bank>`, GET only (`ReadOnly`).

## `/memories/list?time_field=created_at&start_date=<t>&end_date=<t>&limit=<n>&offset=<n>` (`ReadOnly.ListCreatedMemories`)

| Field | Type | Absent | Source |
|---|---|---|---|
| `total` | number | 0 | existing decoder `Memory` listing (`items`, `total`); live probe |
| `items[].id` | string | "" | existing decoder `Memory`; live probe |
| `items[].fact_type` | string | "" | existing decoder `Memory`; live probe |
| `items[].document_id` | string | "" (observations: none) | existing decoder `Memory`; live probe |

Never decoded: `text`, `context`, `entities`, `metadata.*`, `tags`, `chunk_id`, every other field.

The filter honours both bounds (live probe: a 2020 one-day window answered
`total` 0, one recent day a part of the unfiltered total).

## `/documents/<id>` (`ReadOnly.DocumentUnits`)

| Field | Type | Absent | Source |
|---|---|---|---|
| `memory_unit_count` | number | `ok` false (null too) | existing decoder `Document` (`MemoryUnits`); live probe |

Never decoded: `original_text`, or any other field.

## `/operations?status=<s>&type=retain&limit=100&offset=<n>` (`ReadOnly.RetainOperations`)

| Field | Type | Absent | Source |
|---|---|---|---|
| `total` | number | 0 (stops after the page) | existing decoder `OperationPage`; live probe |
| `operations[].id` | string | "" | existing decoder `Operation`; live probe |
| `operations[].operation_type` | string | "" (dropped: retain only) | existing decoder `Operation`; live probe |
| `operations[].status` | string | "" | existing decoder `Operation`; live probe |
| `operations[].document_id` | string | "" | existing decoder `Operation`; live probe |
| `operations[].created_at` | string (ISO time) | zero time | existing decoder `Operation`; live probe |
| `operations[].updated_at` | string (ISO time) | zero time | existing decoder `Operation`; live probe |
| `operations[].retry_count` | number | 0 (null too) | existing decoder `Operation`; live probe |

Never decoded: `error_message`.

## Read unchanged by the collection (existing decoders)

- `/llm-requests/stats?period=30d&operation=retain`: `ReadOnly.LLMStats`.
- `/stats/memories-timeseries?period=90d&time_field=created_at`: `ReadOnly.MemoriesTimeseries`.
- `/tags?q=project:`: `ReadOnly.ProjectTags`.

## Live probe (2026-11-02, field paths)

`[]` stands for any array index.

- `GET /memories/<id>`: `context`, `date`, `entities.[]`, `id`, `mentioned_at`,
  `occurred_end`, `occurred_start`, `source_memories.[].context`,
  `source_memories.[].id`, `source_memories.[].mentioned_at`,
  `source_memories.[].occurred_start`, `source_memories.[].text`,
  `source_memories.[].type`, `source_memory_ids.[]`, `state`, `tags.[]`,
  `text`, `type`. No `created_at`, no `document_id`: this endpoint gives no
  creation time and is not read for origins.
- `GET /memories/list` (unfiltered): `limit`, `offset`, `total`;
  `items.[].chunk_id`, `items.[].consolidated_at`, `items.[].context`,
  `items.[].date`, `items.[].document_id`, `items.[].entities`,
  `items.[].fact_type`, `items.[].id`, `items.[].mentioned_at`,
  `items.[].metadata.kind`, `items.[].metadata.machine_id`,
  `items.[].metadata.session`, `items.[].metadata.source`,
  `items.[].proof_count`, `items.[].source_memory_ids.[]`, `items.[].state`,
  `items.[].tags.[]`, `items.[].text`, `items.[].updated_at`.
- `GET /memories/list?time_field=created_at&start_date=…&end_date=…`: the
  same, without `chunk_id`, `consolidated_at`, `document_id` and `metadata`
  on the rows that have none, and with `occurred_end`, `occurred_start`.
  The list items name the type `fact_type`; the detail endpoint names it
  `type`.
- `GET /documents/<id>`: `memory_unit_count` (number), `created_at`,
  `updated_at`, `nodes_by_fact_type.experience`,
  `nodes_by_fact_type.observation`, `nodes_by_fact_type.world`, and others.
- `GET /operations`: `limit`, `offset`, `total`; `operations.[].created_at`,
  `operations.[].document_id`, `operations.[].id`,
  `operations.[].items_count`, `operations.[].operation_id`,
  `operations.[].operation_type`, `operations.[].retry_count`,
  `operations.[].status`, `operations.[].task_type`,
  `operations.[].updated_at`.
- `GET /llm-requests/stats`: `period`, `start`, `trunc`;
  `buckets.[].statuses.error`, `buckets.[].statuses.success`,
  `buckets.[].time`, `buckets.[].tokens.cached`, `buckets.[].tokens.input`,
  `buckets.[].tokens.output`, `buckets.[].tokens.thoughts`,
  `buckets.[].tokens.total`, `buckets.[].total`.
- `GET /stats/memories-timeseries`: `period`, `time_field`, `trunc`;
  `buckets.[].experience`, `buckets.[].observation`, `buckets.[].time`,
  `buckets.[].world`.
- `GET /tags?q=project:`: `limit`, `offset`, `total` (no items on the
  probed bank).
