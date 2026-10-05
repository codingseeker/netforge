# NetForge examples

`simple-line/experiment.yaml` is a valid Phase 0 configuration for a three
namespace line topology: `client -> router -> server`.

```console
$ netforge config validate examples/simple-line/experiment.yaml
$ netforge config show examples/simple-line/experiment.yaml
```

`invalid/` contains documents that NetForge must reject. Each file exercises one
validation rule and is used by the integration suite to prove that malformed
input is refused rather than silently accepted:

| File | Rule exercised |
| --- | --- |
| `unknown-field.yaml` | unknown top-level fields are rejected |
| `malformed.yaml` | malformed YAML is rejected |
| `invalid-cidr.yaml` | malformed CIDR is rejected |
| `host-bits-cidr.yaml` | a CIDR with host bits is rejected instead of being masked |
| `excessive-limits.yaml` | resource requests above the supported ceilings are rejected |
| `negative-duration.yaml` | negative durations are rejected |

Phase 0 does not implement fault injection, so a document that declares
`faults:` is rejected as an unknown field. The fault schema is introduced in
Phase 4 together with the chaos engine.
