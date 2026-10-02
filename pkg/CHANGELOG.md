# Changelog

## [0.1.1](https://github.com/weaveplatform/weaveplatform-agent-modules/compare/pkg/v0.1.0...pkg/v0.1.1) (2026-10-02)


### Bug Fixes

* **deps:** agent-core sdk 0.12.0 and grpc 1.83.2 (GO-2026-6443) ([2bae969](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/2bae9698111afbf28d8a14f207258dd985091572))
* **deps:** sdk 0.12.0 and grpc 1.83.2; per-OS module CI ([a1600ae](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/a1600ae9bdae20c8a9ef38dcc12acff619ebeb8c))

## 0.1.0 (2026-10-02)


### Features

* **pkg:** product-neutral names for modules, addresses and packages ([26b0740](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/26b07402911c12ae735134ebf31b48375aef814d))
* **pkg:** product-neutral names; capabilities answer whichever peer asked ([7eae60e](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/7eae60e2c460fdff8d85069f497e7e47231991cf))
* **pkg:** shared guest packages with capability-addressed wire contract ([fb6ebe7](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/fb6ebe73951e172b0a0c2c2d0780e6f3e9e86760))
* **pkg:** shared guest packages with capability-addressed wire contract ([f617dfd](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/f617dfd43aa17c5cbda0b06bd69be6ad83b12b49))


### Bug Fixes

* **guestexec:** give ConPTY children null standard handles, not ours ([014cfb4](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/014cfb4736d9d06de5e791d1f74c14bde4b4f13e))
* **guestexec:** keep a terminal's output when its process exits at once ([a9d448d](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/a9d448d19d5c1ac5b3fd04ee5be9371ab01bb18f))
* **guestexec:** let ConPTY output drain before closing the console ([db2b950](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/db2b950ce911e4f3cd44e6ff2097bd616c542a7e))
* **guestexec:** terminal output survives a process that exits at once ([1392ba6](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/1392ba61699a652e6be99207b429b7f769c32c22))
* Windows CI — LF checkouts and ConPTY output drained before close ([be7995d](https://github.com/weaveplatform/weaveplatform-agent-modules/commit/be7995d6ef294c6ff5cbf27b2ad9249d85885991))

## Changelog

release-please maintains this file from conventional commit messages.
