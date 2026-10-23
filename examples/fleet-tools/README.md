# Fleet tools demo

A sealed CPU agent with a script tool and a required secret. The tool reads
the unit's own order file and refuses to answer unless `ORDERS_TOKEN` is set
on the host, so its acceptance tests pass only on a host that has the secret.
The expected answers appear only in that file, so a passing test shows the
tool ran.

On the preparation computer, with `qwen2.5:1.5b` downloaded through Ollama:

```sh
./bin/nexus build examples/fleet-tools --seal
```

On each host, before releasing it:

```sh
./bin/nexus secret set order-desk ORDERS_TOKEN
```

Follow [the Fleet walkthrough](../../docs/FLEET.md) for enrollment and release.
