# Emulate a large fleet

For testing mesh behavior at fleet scale without provisioning one VM per host, a single `dirq-agent` process can host **N virtual hosts** in-process. Each VH presents itself to the server as an independent agent with its own ID, session token, mTLS client cert, upstream gRPC connection, and downstream relay listen port.

```bash
DIRQ_VIRTUAL_HOSTS=25 \
DIRQ_HOSTNAME_PREFIX=dirq-test-linux-1 \
DIRQ_REGISTRATION_JITTER_SECONDS=30 \
./bin/dirq-agent
```

Synthesized hostnames are `<prefix>-NNNNN`. Per-instance mTLS material lives under `$DATA_DIR/tls/instances/<hostname>/` so siblings can't clobber each other. The relay listener binds synchronously in `Run()` before registration, so port collisions surface as a startup error instead of silently failing later.

The AWS test fleet (`make aws`) exposes this via `DIRQ_REPLICAS_PER_VM`:

```bash
LINUX_COUNT=50 DIRQ_REPLICAS_PER_VM=1000 make aws    # 50,000 emulated hosts on 50 VMs
```

The userdata script auto-widens the SG relay port range to `50052..50051+N`, reserves the ephemeral-port block via `net.ipv4.ip_local_reserved_ports` so concurrent `dnf install` doesn't collide with VH listen sockets, and picks a sensible registration-jitter default (N/4 s, clamped to 5–60 s) when running with >1 VH.

!!! note

    Multi-VH is **Linux-only** (Windows VMs stay single-tenant).

!!! warning "Per-VM density caveat"

    Every emulated VH runs its own gRPC stream + state, but they all share the host kernel, CPU, and memory. Running heavy workloads (a real `dnf install`, large package syncs) at 25 VHs/VM on a t3.small saturates the CPU enough that gRPC heartbeats time out and dirq honestly reports VHs as `peer disconnected`. That's a property of the emulation density, not the mesh — production deployments with 1 agent per real host don't have it. For heavy-workload emulation, prefer CPU-rich instance types (`c6i.large`+) or drop density to ~10 VHs/VM.
