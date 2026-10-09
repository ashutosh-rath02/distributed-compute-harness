## What and why

<!-- The problem, then the change. Link an issue if there is one. -->

## How it was tested

<!-- Commands you ran, real-binary runs, what you broke on purpose to see a test fail. -->

## Checklist

- [ ] `make lint` and the relevant tests pass (run with `-p 1`)
- [ ] No new unauthenticated route; no state change on GET
- [ ] Agent/model/relay input validated; anything displayed is escaped
- [ ] Mixed-version fleets still work (feature flag; no catalog version bump)
- [ ] `CHANGELOG.md` updated
