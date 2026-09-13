# Job Groups

**AI Generated**

How to submit many jobs in one request, what a group tells you about them, and what it deliberately does not do.

- [In short](#in-short)
- [What a group is](#what-a-group-is)
- [Submitting a group](#submitting-a-group)
- [Reading a group](#reading-a-group)
- [The group's status](#the-groups-status)
- [Finding a group's jobs](#finding-a-groups-jobs)
- [Dismissing a group](#dismissing-a-group)
- [When submission does not finish](#when-submission-does-not-finish)
- [How members are scheduled](#how-members-are-scheduled)
- [Configuring the server](#configuring-the-server)
- [Restarts and recovery](#restarts-and-recovery)
- [Limitations](#limitations)
- [Troubleshooting](#troubleshooting)

## In short

Submit many executions of one process in a single request:

```sh
curl -X POST http://localhost:5050/processes/pyecho/group-execution \
  -H 'Content-Type: application/json' \
  -d '{
        "tags": ["reach:1269877024692972"],
        "jobs": [
          {"inputs": {"text": "scenario 400"}, "tags": ["q:400"]},
          {"inputs": {"text": "scenario 550"}, "tags": ["q:550"]}
        ]
      }'
```

You get one group ID back. Poll it for a combined status, and dismiss every
unfinished member with one call:

```sh
curl http://localhost:5050/job-groups/0d8f6c2e-4b1a-4f7e-9a53-2c6e1b8f4d10
curl -X DELETE http://localhost:5050/job-groups/0d8f6c2e-4b1a-4f7e-9a53-2c6e1b8f4d10
```

## What a group is

A group is a submission and tracking mechanism, nothing more. Every member is
an ordinary SEPEX job with its own status, logs, results and metadata at
`/jobs/{jobID}`, and it runs exactly as it would have if you had submitted it on
its own.

A group does:

- submit its jobs in one request
- report a combined status, and a count of its members by status
- list its members in submission order
- dismiss every unfinished member in one call

A group does not:

- serve logs, results or metadata. Read those from each member.
- order its members or make them depend on each other. The queue decides when
  each one runs, exactly as it would otherwise.
- change how any member is queued, allocated GPUs, or submitted to AWS Batch.

A group is not an OGC job. It has no logs or results, so it could not honestly
claim the type an OGC status document requires, which is why it lives at
`/job-groups/{groupID}` rather than under `/jobs`.

## Submitting a group

`POST /processes/{processID}/group-execution`

The body is the group's own tags, plus one entry per job:

```json
{
  "tags": ["reach:1269877024692972"],
  "jobs": [
    {"inputs": {"text": "scenario 400"}, "tags": ["q:400"]},
    {"inputs": {"text": "scenario 550"}, "tags": ["q:550"]}
  ]
}
```

`inputs` is required for every entry and is validated against the process the
same way a single execution is. `tags` is optional on the group and on each
entry; the group's tags are written onto every member, alongside that member's
own.

Every member runs the same process. Submitting against a process needs the same
permission as executing it once: an admin, or the role named after the process.

**The whole request is validated before anything is created.** If any entry has
bad inputs, if a tag is malformed, if the process cannot run asynchronously, or
if the group is larger than `MAX_GROUP_SIZE`, the request fails with a 4xx and
no job is created. Fix it and send it again.

The response is `201 Created` as soon as the group is recorded:

```json
{
  "groupID": "0d8f6c2e-4b1a-4f7e-9a53-2c6e1b8f4d10",
  "status": "accepted",
  "created": "2026-09-11T18:02:11Z",
  "submitter": "someone@example.com",
  "tags": ["reach:1269877024692972"],
  "requested": 2,
  "links": [{"href": "/job-groups/0d8f6c2e-...", "rel": "self", "type": "application/json"}]
}
```

**The members do not exist yet.** They are created in the background, because a
large group takes far longer to submit than a request should stay open: on
`aws-batch` every member costs a submission round trip. So the response promises
that the group was accepted, not that its jobs are running. Watch them appear
through `GET /job-groups/{groupID}`.

Groups are always asynchronous. A process that does not offer `async-execute` is
refused with `422`, and a `Prefer` header has nothing to choose between.

## Reading a group

`GET /job-groups/{groupID}`

```json
{
  "groupID": "0d8f6c2e-4b1a-4f7e-9a53-2c6e1b8f4d10",
  "status": "running",
  "created": "2026-09-11T18:02:11Z",
  "updated": "2026-09-11T18:09:40Z",
  "submitter": "someone@example.com",
  "tags": ["reach:1269877024692972"],
  "requested": 10,
  "summary": { "accepted": 3, "running": 4, "successful": 2,
               "failed": 1, "dismissed": 0, "lost": 0, "notCreated": 0 },
  "jobs": [
    { "position": 0, "jobID": "5a0e2f7c-…", "processID": "runKwseScenariosLisfloodGpu",
      "status": "successful", "updated": "2026-09-11T18:06:02Z",
      "tags": ["group:0d8f6c2e-…", "reach:1269877024692972", "q:400"] }
  ],
  "links": [{"href": "/job-groups/0d8f6c2e-…", "rel": "self", "type": "application/json"}]
}
```

- `summary` counts every member of the group, whatever page you asked for.
- `requested` is how many jobs the submission asked for, and never changes.
- `updated` is the latest member update, or the dismissal if that came later.
- `jobs` is one page of members in submission order. Everything else about a
  member is at `/jobs/{jobID}`.

`limit` (default 20, maximum 100) and `offset` page through the members. A
`limit` above the maximum is clamped to it rather than ignored. `prev` and `next`
links are included when there is a page to go to.

`status` filters the members, comma separated. It takes any job status, plus
`notCreated` for members that have no job at all:

```sh
# which members failed
curl 'http://localhost:5050/job-groups/0d8f6c2e-…?status=failed'

# which entries of my request were never created, so I can resubmit them
curl 'http://localhost:5050/job-groups/0d8f6c2e-…?status=notCreated'
```

Filtering changes the members listed, never the summary. Like every other GET in
SEPEX, this one answers in HTML or JSON depending on the caller, or on `?f=html`
and `?f=json`.

At `AUTH_LEVEL=2` a group follows the rule of the job list rather than of a
single job: a non-admin sees only the groups they submitted.

## The group's status

A group is never reported as finished while its members are still being created,
however the ones created so far have turned out. Otherwise a group whose first
few members happened to succeed would claim success while the rest of the work
was still being submitted.

Once nothing further is coming, a group is only as good as its worst member:

| Members                                          | Group        |
| ------------------------------------------------ | ------------ |
| every member still queued                        | `accepted`   |
| 3 running · 7 accepted                           | `running`    |
| 2 successful · 1 failed · 4 running · 3 accepted | `running`    |
| 10 successful                                    | `successful` |
| 9 successful · 1 failed                          | `failed`     |
| 9 successful · 1 lost                            | `failed`     |
| 9 successful · 1 that could not be created       | `failed`     |
| 6 successful · 4 dismissed                       | `dismissed`  |
| 7 successful · 2 dismissed · 1 failed            | `failed`     |

There is no fail-fast. A group reports `failed` once its members have all
finished, not at the first failure, so the members that can still do useful work
do it.

A member that could not be created counts as a failure of the group, because the
work it was asked to do does not exist. A lost member counts the same way. The
`summary` always says which of these actually happened.

## Finding a group's jobs

Every member is tagged `group:{groupID}`, so a group's jobs can be found through
the ordinary job list:

```sh
curl 'http://localhost:5050/jobs?tags=group:0d8f6c2e-4b1a-4f7e-9a53-2c6e1b8f4d10&limit=100'
```

The `group:` prefix is reserved. A tag starting with it is refused on both the
group and the single job endpoints, so a job cannot claim to belong to a group
it is not in.

## Dismissing a group

`DELETE /job-groups/{groupID}`

Dismisses every member that is still accepted or running, through the same code
as `DELETE /jobs/{jobID}`: a queued member leaves the local queue and releases
the resources reserved for it, a running one is stopped on its host. Members
that have already finished are left exactly as they are, with their logs and
results intact.

If the group is still being submitted, the submission stops first, so no member
is created behind the dismissal.

Members are dismissed in reverse submission order. Running members hold the
earliest positions, so working backwards clears the queue of the group's members
before the first running one releases its resources. Otherwise the queue would
start the next member of the very group being dismissed, only for it to be
killed moments later.

The call is safe to repeat: another call retries any member that could not be
dismissed the first time, and reports which ones those were.

## When submission does not finish

A member that cannot be created does not stop the members after it. Killing jobs
that are running perfectly well because a later one failed would throw away real
work, so nothing already created is touched.

Instead, the position keeps its place in the group and carries the reason:

```json
{ "position": 41, "error": "aws batch: ... request too large", "tags": [] }
```

Submission stops early only when it is clearly not going to recover: after 10
consecutive failures, which is what a database being down or credentials having
expired looks like. The positions never reached are recorded as not attempted,
so every entry of your request can be accounted for.

Either way the group ends up with `notCreated` members, reports `failed` once
everything else is finished, and carries a message saying how many could not be
created and why the first one failed. `?status=notCreated` lists exactly which
positions to resubmit; positions match the order of the `jobs` array you sent.

SEPEX never dismisses a group's members on its own. A group that failed to
submit completely leaves its members running, and it is up to you whether to keep
the partial result or `DELETE` the group.

## How members are scheduled

Members join the local FIFO queue in submission order and are admitted like any
other job, each with its own resources and its own GPU allocation. A group has no
priority of its own: its members simply arrived when they arrived, so they run
before jobs submitted after them and after jobs submitted before them.

One consequence is worth knowing on a GPU host. The queue admits jobs strictly in
order, so while a member is waiting at the front of the queue for a GPU, jobs
behind it wait too, even ones that need no GPU and could run on otherwise idle
CPUs. A large group makes that wait longer. This is how the queue already
behaves for any run of submissions; a group just makes it easy to create one.

On `aws-batch`, each member is its own Batch job, submitted during the group's
background submission, and the status Lambda reports each one by its own job
name. Members of one group may even run on different host types without any
special handling, since each is an ordinary job.

## Configuring the server

| Variable         | Default | What it does                                                                 |
| ---------------- | ------- | ---------------------------------------------------------------------------- |
| `MAX_GROUP_SIZE` | `1000`  | The most jobs one group may ask for. A larger request is refused with `400`. |

The limit exists because a group is one request that turns into many jobs. On
`aws-batch` each member costs a `SubmitJob` call against an account-wide rate
limit shared with every other submission, so a very large group slows down
everyone's jobs while it submits. Locally, each member costs database writes and
a queue slot. Raise it if your workload needs to and your hosts can take it.

At most four groups are submitted at once. A group waiting its turn stays in
submission for longer, which its status already shows.

## Restarts and recovery

Groups hold nothing in memory. Everything a group knows is in the database, so a
restart has nothing to rebuild, and the members recover exactly as they do today
(see the Recovery section of DEV_GUIDE.md).

If the server stops while a group is still being submitted, that group is closed
out at the next startup: it is marked as no longer submitting, with a message
saying how many members were created. The members that were created are left
alone. The ones that were not are counted as `notCreated`, so the group reports
`failed`, which is the honest answer — the work you asked for is not all there.

Recovery dismisses local jobs that were still queued when the server stopped, so
a group whose members were waiting can report `dismissed` without anyone having
called `DELETE`.

## Limitations

- **One process per group.** Every member runs the same process. Submit a second
  group for a second process.
- **No ordering or dependencies.** If the work has to happen in sequence, that
  sequence belongs inside one job.
- **No group logs, results or metadata.** Read them from each member.
- **No group list or search.** A group is reachable by its ID. Its members are
  searchable through `/jobs` by the `group:` tag.
- **No deduplication of retries.** Group IDs are generated by the server, so
  sending the same submission twice creates two groups and runs the work twice.
  Record the group ID from the response.
- **Synchronous execution is not available.** Groups are always asynchronous.

## Troubleshooting

| What you see                                            | What it means                                                                                        |
| ------------------------------------------------------- | ---------------------------------------------------------------------------------------------------- |
| `422` naming `async-execute`                            | The process only supports synchronous execution. Groups are always asynchronous, so add `async-execute` to its `jobControlOptions`. |
| `400` about `MAX_GROUP_SIZE`                            | The request asked for more jobs than the server allows. Split it, or raise `MAX_GROUP_SIZE`.         |
| `400` naming a job by number                            | That entry of the `jobs` array failed validation. The number is its position in the array. Nothing was created. |
| `400` about a reserved tag                              | A tag starts with `group:`, which only the server may write.                                         |
| The group is `accepted` and `jobs` is empty             | Submission has not reached its first member yet, or is waiting for one of the four submission slots. |
| `jobs` has fewer entries than `requested`               | Members are still being created. Once `notCreated` is above zero in the summary, the rest are not coming. |
| Members are `accepted` and never start                  | The queue is full, or a member at the front of the queue is waiting for a GPU. Check `/admin/resources`. |
| The group is `dismissed` but nobody dismissed it        | A restart dismissed members that were still queued. See Restarts and recovery.                       |
| `404` on something like `/job-groups/{groupID}/results` | There is no such path: a group has no logs, results or metadata of its own. Read them from each member at `/jobs/{jobID}`. |