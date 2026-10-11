# Example: count the words of a big file across devices

Cut `big.txt` into 8 chunks, count each chunk on whichever device is free, and add the counts up.

```
harnessctl mapreduce -in big.txt -chunks 8 \
    -map text.count \
    -reduce "json.merge mode=sum field=total"
harnessctl workflow show <workflow-id>
harnessctl workflow outputs <workflow-id> out/       # out/merged.json
```

`merged.json`:

```json
{ "bytes": 1048576, "lines": 20000, "words": 180000 }
```

It is the same as `text.count` on the whole file (`harnessctl do text.count -in big.txt`): `text.split` cuts only at line ends, so lines, words and bytes add up exactly (this is checked in the tests).

## What it builds

A two-step workflow (`harnessctl mapreduce` prints nothing you could not write by hand):

```json
{
  "stages": [
    {"type": "text.split", "mode": "once", "params": {"chunks": "8", "by": "lines"}},
    {"type": "text.count", "mode": "perFile",
     "combine": {"type": "json.merge", "params": {"mode": "sum", "field": "total"}}}
  ]
}
```

1. **split** (`text.split`): one run writes `chunk-0001.txt` ... `chunk-0008.txt`. By `lines` the chunks have equal line counts, by `bytes` about equal sizes (always cut after a newline). It always writes exactly N files; a file with fewer lines than chunks leaves the last ones empty. **At most 16 chunks**: a task may declare at most 16 output files.
2. **map** (`text.count`, one run per chunk): spread over the fleet, retried on failure; each writes `counts.json`.
3. **reduce** (`json.merge`, the stage's `combine`): gets every chunk's `counts.json` as `parts/0000/counts.json` ... and merges them.

Any task that takes one text file can be the map, and any reduce works on its results; for example `-map "llm.generate model=llama3.2:3b prompt=Summarize"` with `-reduce "json.merge mode=collect"` needs the map to write JSON.

## `json.merge`

| mode | what it does |
|---|---|
| `sum` (default) | adds the files: numbers are added (exact for whole numbers, overflow is an error), objects are merged key by key recursively, arrays are joined. Anything else is an error naming the path. |
| `concat` | every file must be a JSON array; the result is one array of all their elements |
| `collect` | one object keyed by input file name, holding each file's JSON |

`field=total` (or `a.b.c`) takes that sub-object of each file first. Files are taken in name order, so the result does not depend on how the parts were listed. Limits: 8 MiB per file, 64 MiB in all, 32 levels deep.

Both types are deterministic (same bytes on every device), so they take part in spot checks. The AI planner may use them like any catalog type: their parameters fit its schema, and a plan is checked by the same workflow check before anyone approves it.
