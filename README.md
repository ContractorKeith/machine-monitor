# machine-monitor

A terminal app that watches CPU, GPU, memory, temperatures, disks, network, processes and
power on one machine, or on every machine on your network, from a single binary on Windows,
macOS and Linux.

**Status:** early development (M0 skeleton). Nothing usable yet. The build plan is in [docs/PLAN.md](docs/PLAN.md).

## Credit

machine-monitor reimplements, as a TUI, the features of
[Device Monitor](https://github.com/Forward-Future/device-monitor) by Matt / Forward Future
(MIT). Logic ported from it (the power model, electricity price table, alert rules and report
validation) keeps its MIT notice in the source files it lands in.

## License

MIT. See [LICENSE](LICENSE).
