# Server shapes the collection reads

Field paths and JSON types only, never values. Each field the code relies on
names its source: an existing decoder of this package, or the field name
assumed from the task when no decoder read it before. The live probe's
paths are appended below once it has run.

All paths are under `/v1/default/banks/<bank>`, GET only (`ReadOnly`).

## `/memories/<id>` (`ReadOnly.MemoryOrigin`)

| Field | Type | Absent | Source |
|---|---|---|---|
| `id` | string | "" | existing decoder `MemoryDetail` |
| `document_id` | string | "" | existing decoder `MemoryDetail` |
| `type` | string (fact type) | "" | existing decoder `MemoryDetail` |
| `created_at` | string (ISO time, zone optional) | zero time; never `mentioned_at` | assumed (brief) |

Never decoded: `text`, `source_memories[].text`, `history`, `tags`, `mentioned_at`.

## `/documents/<id>` (`ReadOnly.DocumentUnits`)

| Field | Type | Absent | Source |
|---|---|---|---|
| `memory_unit_count` | number | `ok` false (null too) | existing decoder `Document` (`MemoryUnits`); presence assumed (brief) |

Never decoded: `original_text`, or any other field.

## `/operations?status=<s>&type=retain&limit=100&offset=<n>` (`ReadOnly.RetainOperations`)

| Field | Type | Absent | Source |
|---|---|---|---|
| `total` | number | 0 (stops after the page) | existing decoder `OperationPage` |
| `operations[].id` | string | "" | existing decoder `Operation` |
| `operations[].operation_type` | string | "" (dropped: retain only) | existing decoder `Operation` |
| `operations[].status` | string | "" | existing decoder `Operation` |
| `operations[].document_id` | string | "" | existing decoder `Operation` |
| `operations[].created_at` | string (ISO time) | zero time | existing decoder `Operation` |
| `operations[].updated_at` | string (ISO time) | zero time | existing decoder `Operation` |
| `operations[].retry_count` | number | 0 (null too) | existing decoder `Operation` |

Never decoded: `error_message`.

## Read unchanged by the collection (existing decoders)

- `/llm-requests/stats?period=30d&operation=retain`: `ReadOnly.LLMStats`.
- `/stats/memories-timeseries?period=90d&time_field=created_at`: `ReadOnly.MemoriesTimeseries`.
- `/tags?q=project:`: `ReadOnly.ProjectTags`.

## Live probe

Not run yet.
