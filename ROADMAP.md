# Roadmap

Where box is going. Each milestone is a [GitHub milestone][ms] with one issue
per deliverable, and [#16] is the index. Milestones ship in order; there are
no dates.

## Shipped

- **v0.2:** Boxfile validation, declarative `mounts:`, `restore`, named
  snapshots, isolation re-checked on every run.
- **v0.3:** VMM confinement and `isolation: strict`, the escape suite,
  `up`/`exec`, the warm pool, forks, three backends, `box data`, SDKs for
  Python, TypeScript, Go and Rust, the MCP server.
- **v0.4:** bluebox renamed to box; safer restore, govulncheck, Dependabot and
  fuzzing in CI since.

## Next

- **v0.5, sandboxes isolated from each other:** a UID per strict sandbox
  ([#27], done), `box inspect --security-report` ([#28], done), `backend: auto` ([#29], done),
  cross-sandbox escape tests ([#30]), a `/data` size limit ([#10], done), the nightly
  escape suite running again ([#31]).
- **v0.6, network policy:** egress allowlists through a forced proxy ([#7]), a
  connection log ([#8]), networking for Firecracker ([#32]).
- **v0.7, live forks:** memory checkpoints and live forks ([#33]), reproducible
  runs ([#34]).
- **v0.8, supply chain and robustness:** builds inside a VM ([#35]), signed
  releases ([#36]), pinned images ([#37]), more fuzzing ([#38]), fault and soak
  tests ([#39]), benchmarks under concurrency ([#40]), an in-guest process limit
  ([#53]).
- **v0.9, platforms and interfaces:** macOS through Lima ([#41]), a gRPC API
  ([#42]), packages and published SDKs ([#43]), logs and metrics ([#44]), a
  fuller guest kernel for Kubernetes ([#9]).
- **v1.0, production:** multi-tenant server mode ([#45]), an external security
  review ([#46]), a Kata Containers backend for Kubernetes, GPUs and
  confidential VMs ([#48]), box on Kubernetes: an operator, a `Sandbox`
  resource and a Helm chart ([#49]).

[ms]: https://github.com/chxperiments/box/milestones
[#7]: https://github.com/chxperiments/box/issues/7
[#8]: https://github.com/chxperiments/box/issues/8
[#9]: https://github.com/chxperiments/box/issues/9
[#10]: https://github.com/chxperiments/box/issues/10
[#16]: https://github.com/chxperiments/box/issues/16
[#27]: https://github.com/chxperiments/box/issues/27
[#28]: https://github.com/chxperiments/box/issues/28
[#29]: https://github.com/chxperiments/box/issues/29
[#30]: https://github.com/chxperiments/box/issues/30
[#31]: https://github.com/chxperiments/box/issues/31
[#32]: https://github.com/chxperiments/box/issues/32
[#33]: https://github.com/chxperiments/box/issues/33
[#34]: https://github.com/chxperiments/box/issues/34
[#35]: https://github.com/chxperiments/box/issues/35
[#36]: https://github.com/chxperiments/box/issues/36
[#37]: https://github.com/chxperiments/box/issues/37
[#38]: https://github.com/chxperiments/box/issues/38
[#39]: https://github.com/chxperiments/box/issues/39
[#40]: https://github.com/chxperiments/box/issues/40
[#41]: https://github.com/chxperiments/box/issues/41
[#42]: https://github.com/chxperiments/box/issues/42
[#43]: https://github.com/chxperiments/box/issues/43
[#44]: https://github.com/chxperiments/box/issues/44
[#45]: https://github.com/chxperiments/box/issues/45
[#46]: https://github.com/chxperiments/box/issues/46
[#48]: https://github.com/chxperiments/box/issues/48
[#53]: https://github.com/chxperiments/box/issues/53
[#49]: https://github.com/chxperiments/box/issues/49
