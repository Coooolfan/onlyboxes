# Worker Boxlite Overview

Boxlite is a local-first micro-VM sandbox for **AI agents**. It is stateful, lightweight,
provides hardware-level isolation, and **requires no daemon**.

This worker implements the Onlyboxes worker protocol to connect to the console and execute tasks in Boxlite VMs.

Boxlite is a Rust-based project. Its source code is available at https://github.com/boxlite-ai/boxlite.

Notes:
- For local development, the Boxlite source repository must be cloned to `~/Documents/code/boxlite`.
- If the source code is not available locally, ask the user to clone it first.
- If the Boxlite SDK does not provide functionality required by this worker, forking Boxlite is permitted as a last resort. However, the user must confirm the requirement and coordinate the development roadmap of both Onlyboxes and Boxlite accordingly.
