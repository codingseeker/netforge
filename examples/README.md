# NetForge examples

Both valid documents grant the namespaces a run creates when it is given
`--id demo`, so run them with that identifier. The laboratory identifier
otherwise defaults to a value derived from the document itself, and the
namespaces it builds would not be the ones the document names.

`simple-line/experiment.yaml` is a valid configuration for a three namespace line
topology: client, router and server.

```console
$ netforge config validate examples/simple-line/experiment.yaml
$ netforge config show examples/simple-line/experiment.yaml
$ netforge experiment run --id demo \
    --namespace nf-client0-demo --target 10.42.0.6 --protocol icmp \
    --recover remove_fault:nf-server0-demo --authorize \
    --why "the example removes the loss its own document declared" \
    examples/simple-line/experiment.yaml
```

`cascading-failure/experiment.yaml` is a valid configuration for a larger scope —
two servers, a router and a load balancer namespace — that declares packet loss
in its `faults:` block. A plain run builds one client and one server, so it uses
the namespaces of those two; `--servers 2 --balancer 2` builds the rest.

```console
$ netforge config validate examples/cascading-failure/experiment.yaml
$ netforge experiment plan --namespace nf-client0-demo --target 10.42.0.6 \
    --protocol icmp examples/cascading-failure/experiment.yaml
```

`--id` belongs to `experiment run`: a plan is compiled from the document alone,
so it does not build a laboratory and does not take an identifier.

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
