# Fleet demo

A small sealed CPU chat agent with two acceptance checks. Follow
[the Fleet walkthrough](../../docs/FLEET.md) for runtime setup, enrollment,
canary release, local execution, and rollback.

On the preparation computer, with `qwen2.5:0.5b` downloaded through Ollama:

```sh
./bin/nexus build examples/fleet-demo --seal
```

`nexus.yaml` defines the agent; `evals/suite.yaml` travels inside the artifact
and runs on each host before activation. Keep test inputs synthetic because
the suite is part of the published unit.
