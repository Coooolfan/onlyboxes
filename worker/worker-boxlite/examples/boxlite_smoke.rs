//! Standalone smoke test for the Boxlite backend.
//!
//! Exercises the full box lifecycle directly against the `boxlite` crate API,
//! without console, gRPC, or any worker-boxlite internals. Used to validate a
//! boxlite dependency upgrade on real hardware.
//!
//! Run with:
//!   cargo run --example boxlite_smoke
//!
//! Environment overrides:
//!   BOXLITE_SMOKE_IMAGE        rootfs image            (default: alpine:latest)
//!   BOXLITE_SMOKE_HOME         boxlite home dir        (default: a fresh temp dir)
//!   BOXLITE_SMOKE_CONCURRENCY  parallel exec count     (default: 4)
//!   BOXLITE_SMOKE_KEEP_HOME    keep home dir on exit   (default: unset)

use std::path::PathBuf;
use std::sync::Arc;
use std::time::{Duration, Instant};

use boxlite::{
    BoxCommand, BoxOptions, BoxliteOptions, BoxliteRuntime, CopyOptions, LiteBox, RootfsSpec,
};
use tokio_stream::StreamExt;

/// Per-exec sleep used by the concurrency check, in seconds.
const CONCURRENCY_SLEEP_SECS: u64 = 2;

struct Report {
    passed: Vec<String>,
    failed: Vec<(String, String)>,
}

impl Report {
    fn new() -> Self {
        Self {
            passed: Vec::new(),
            failed: Vec::new(),
        }
    }

    fn pass(&mut self, step: &str, detail: impl AsRef<str>) {
        let detail = detail.as_ref();
        if detail.is_empty() {
            println!("  PASS  {step}");
        } else {
            println!("  PASS  {step} ({detail})");
        }
        self.passed.push(step.to_owned());
    }

    fn fail(&mut self, step: &str, detail: impl AsRef<str>) {
        let detail = detail.as_ref().to_owned();
        println!("  FAIL  {step}: {detail}");
        self.failed.push((step.to_owned(), detail));
    }

    fn check(&mut self, step: &str, ok: bool, detail: impl AsRef<str>) {
        if ok {
            self.pass(step, detail);
        } else {
            self.fail(step, detail);
        }
    }
}

/// Output collected from a single execution.
struct ExecOutput {
    stdout: String,
    stderr: String,
    exit_code: i32,
    execution_id: String,
}

/// Runs a command to completion, collecting stdout/stderr concurrently with the
/// wait so the guest never blocks on a full pipe.
async fn run(litebox: &LiteBox, command: BoxCommand) -> Result<ExecOutput, String> {
    let mut execution = litebox
        .exec(command)
        .await
        .map_err(|err| format!("exec failed: {err}"))?;

    let execution_id = execution.id().as_str().to_owned();
    let stdout_stream = execution.stdout();
    let stderr_stream = execution.stderr();

    let stdout_task = tokio::spawn(async move {
        let mut buf = String::new();
        if let Some(mut stream) = stdout_stream {
            while let Some(chunk) = stream.next().await {
                buf.push_str(&chunk);
            }
        }
        buf
    });
    let stderr_task = tokio::spawn(async move {
        let mut buf = String::new();
        if let Some(mut stream) = stderr_stream {
            while let Some(chunk) = stream.next().await {
                buf.push_str(&chunk);
            }
        }
        buf
    });

    let result = execution
        .wait()
        .await
        .map_err(|err| format!("wait failed: {err}"))?;

    let stdout = stdout_task.await.unwrap_or_default();
    let mut stderr = stderr_task.await.unwrap_or_default();
    if let Some(message) = result.error_message.as_deref().map(str::trim) {
        if !message.is_empty() {
            if !stderr.is_empty() && !stderr.ends_with('\n') {
                stderr.push('\n');
            }
            stderr.push_str(message);
        }
    }

    Ok(ExecOutput {
        stdout,
        stderr,
        exit_code: result.exit_code,
        execution_id,
    })
}

fn shell(command: &str) -> BoxCommand {
    BoxCommand::new("sh").args(["-lc", command])
}

#[tokio::main]
async fn main() {
    let image = std::env::var("BOXLITE_SMOKE_IMAGE").unwrap_or_else(|_| "alpine:latest".to_owned());
    let concurrency: usize = std::env::var("BOXLITE_SMOKE_CONCURRENCY")
        .ok()
        .and_then(|value| value.parse().ok())
        .filter(|value| *value > 1)
        .unwrap_or(4);
    let keep_home = std::env::var("BOXLITE_SMOKE_KEEP_HOME").is_ok();

    let (home_dir, home_is_temp) = match std::env::var("BOXLITE_SMOKE_HOME") {
        Ok(value) if !value.trim().is_empty() => (PathBuf::from(value.trim()), false),
        _ => (
            std::env::temp_dir().join(format!(
                "boxlite-smoke-{}-{:08x}",
                std::process::id(),
                rand::random::<u32>()
            )),
            true,
        ),
    };

    println!("boxlite smoke test");
    println!("  image       {image}");
    println!("  home_dir    {}", home_dir.display());
    println!("  concurrency {concurrency}");
    println!();

    let mut report = Report::new();
    let box_name = format!("smoke-{:08x}", rand::random::<u32>());

    // --- 1. runtime init (covers on-disk database creation / migration) ---
    let options = BoxliteOptions {
        home_dir: home_dir.clone(),
        ..Default::default()
    };
    let runtime = match BoxliteRuntime::new(options) {
        Ok(runtime) => {
            report.pass("runtime init", "");
            Arc::new(runtime)
        }
        Err(err) => {
            report.fail("runtime init", err.to_string());
            summarize(&report, &home_dir, home_is_temp, keep_home);
            return;
        }
    };

    // --- 2. create + start ---
    let started = Instant::now();
    let litebox = match runtime
        .create(
            BoxOptions {
                cpus: Some(1),
                memory_mib: Some(512),
                rootfs: RootfsSpec::Image(image.clone()),
                auto_delete: Some(0),
                ..Default::default()
            },
            Some(box_name.clone()),
        )
        .await
    {
        Ok(litebox) => {
            report.pass("box create", format!("{:?}", started.elapsed()));
            Arc::new(litebox)
        }
        Err(err) => {
            report.fail("box create", err.to_string());
            summarize(&report, &home_dir, home_is_temp, keep_home);
            return;
        }
    };

    let box_id = litebox.id().as_str().to_owned();
    let started = Instant::now();
    if let Err(err) = litebox.start().await {
        report.fail("box start", err.to_string());
        let _ = runtime.remove(&box_id, true).await;
        summarize(&report, &home_dir, home_is_temp, keep_home);
        return;
    }
    report.pass("box start", format!("{:?}", started.elapsed()));

    // --- 3. basic exec ---
    match run(litebox.as_ref(), shell("echo hello-from-boxlite")).await {
        Ok(output) => report.check(
            "exec stdout",
            output.exit_code == 0 && output.stdout.contains("hello-from-boxlite"),
            format!(
                "exit={} stdout={:?} stderr={:?}",
                output.exit_code,
                output.stdout.trim(),
                output.stderr.trim()
            ),
        ),
        Err(err) => report.fail("exec stdout", err),
    }

    // --- 4. non-zero exit and stderr propagation ---
    match run(litebox.as_ref(), shell("echo to-stderr >&2; exit 42")).await {
        Ok(output) => report.check(
            "exec exit code + stderr",
            output.exit_code == 42 && output.stderr.contains("to-stderr"),
            format!(
                "exit={} stderr={:?}",
                output.exit_code,
                output.stderr.trim()
            ),
        ),
        Err(err) => report.fail("exec exit code + stderr", err),
    }

    // --- 5. filesystem state persists across executions ---
    let persisted = match run(
        litebox.as_ref(),
        shell("mkdir -p /workspace && echo persisted > /workspace/state.txt"),
    )
    .await
    {
        Ok(output) if output.exit_code == 0 => {
            match run(litebox.as_ref(), shell("cat /workspace/state.txt")).await {
                Ok(read) => {
                    report.check(
                        "filesystem state shared across execs",
                        read.exit_code == 0 && read.stdout.contains("persisted"),
                        format!("stdout={:?}", read.stdout.trim()),
                    );
                    read.exit_code == 0
                }
                Err(err) => {
                    report.fail("filesystem state shared across execs", err);
                    false
                }
            }
        }
        Ok(output) => {
            report.fail(
                "filesystem state shared across execs",
                format!("write failed: exit={}", output.exit_code),
            );
            false
        }
        Err(err) => {
            report.fail("filesystem state shared across execs", err);
            false
        }
    };

    // --- 6. concurrent exec on one box (core assumption of the refactor) ---
    let serial_lower_bound = Duration::from_secs(CONCURRENCY_SLEEP_SECS * concurrency as u64);
    let started = Instant::now();
    let mut handles = Vec::new();
    for index in 0..concurrency {
        let litebox = litebox.clone();
        handles.push(tokio::spawn(async move {
            run(
                litebox.as_ref(),
                shell(&format!(
                    "sleep {CONCURRENCY_SLEEP_SECS}; echo done-{index}"
                )),
            )
            .await
            .map(|output| (index, output))
        }));
    }

    let mut outputs = Vec::new();
    let mut concurrent_error = None;
    for handle in handles {
        match handle.await {
            Ok(Ok(entry)) => outputs.push(entry),
            Ok(Err(err)) => concurrent_error = Some(err),
            Err(err) => concurrent_error = Some(format!("task panicked: {err}")),
        }
    }
    let elapsed = started.elapsed();

    if let Some(err) = concurrent_error {
        report.fail("concurrent exec", err);
    } else {
        let all_ok = outputs.len() == concurrency
            && outputs.iter().all(|(index, output)| {
                output.exit_code == 0 && output.stdout.contains(&format!("done-{index}"))
            });
        report.check(
            "concurrent exec results",
            all_ok,
            format!("{}/{} succeeded", outputs.len(), concurrency),
        );

        let mut ids: Vec<&str> = outputs
            .iter()
            .map(|(_, output)| output.execution_id.as_str())
            .collect();
        ids.sort_unstable();
        let before = ids.len();
        ids.dedup();
        report.check(
            "distinct execution ids",
            ids.len() == before,
            format!("{} unique of {before}", ids.len()),
        );

        // Serialized execution would take at least concurrency * sleep.
        report.check(
            "concurrent exec is actually parallel",
            elapsed < serial_lower_bound,
            format!("{elapsed:?} < serial lower bound {serial_lower_bound:?}"),
        );
    }

    // --- 7. kill isolation: killing one execution must not disturb another ---
    {
        let victim_box = litebox.clone();
        let victim = tokio::spawn(async move {
            let execution = victim_box
                .exec(shell("sleep 30"))
                .await
                .map_err(|err| format!("victim exec failed: {err}"))?;
            tokio::time::sleep(Duration::from_millis(500)).await;
            execution
                .kill()
                .await
                .map_err(|err| format!("victim kill failed: {err}"))?;
            execution
                .wait()
                .await
                .map_err(|err| format!("victim wait failed: {err}"))
        });

        let survivor_box = litebox.clone();
        let survivor = tokio::spawn(async move {
            run(survivor_box.as_ref(), shell("sleep 3; echo survived")).await
        });

        let victim_result = victim.await;
        match survivor.await {
            Ok(Ok(output)) => report.check(
                "kill isolates to its own execution",
                output.exit_code == 0 && output.stdout.contains("survived"),
                format!(
                    "survivor exit={} stdout={:?}, victim={}",
                    output.exit_code,
                    output.stdout.trim(),
                    match &victim_result {
                        Ok(Ok(result)) => format!("exit={}", result.exit_code),
                        Ok(Err(err)) => format!("error: {err}"),
                        Err(err) => format!("panicked: {err}"),
                    }
                ),
            ),
            Ok(Err(err)) => report.fail("kill isolates to its own execution", err),
            Err(err) => report.fail(
                "kill isolates to its own execution",
                format!("task panicked: {err}"),
            ),
        }
    }

    // --- 8. copy_out ---
    if persisted {
        let host_dst = home_dir.join("copied-state.txt");
        let copy_options = CopyOptions::default().non_recursive().include_parent(false);
        match litebox
            .copy_out("/workspace/state.txt", &host_dst, copy_options)
            .await
        {
            Ok(()) => {
                let candidate = if host_dst.is_dir() {
                    host_dst.join("state.txt")
                } else {
                    host_dst.clone()
                };
                match std::fs::read_to_string(&candidate) {
                    Ok(content) => report.check(
                        "copy_out file content",
                        content.contains("persisted"),
                        format!("{} -> {:?}", candidate.display(), content.trim()),
                    ),
                    Err(err) => report.fail(
                        "copy_out file content",
                        format!("read {} failed: {err}", candidate.display()),
                    ),
                }
            }
            Err(err) => report.fail("copy_out file content", err.to_string()),
        }
    } else {
        report.fail("copy_out file content", "skipped: source file not created");
    }

    // --- 9. remove ---
    match runtime.remove(&box_id, true).await {
        Ok(()) => match runtime.exists(&box_id).await {
            Ok(false) => report.pass("box remove", "exists() == false"),
            Ok(true) => report.fail("box remove", "box still exists after remove"),
            Err(err) => report.fail("box remove", format!("exists check failed: {err}")),
        },
        Err(err) => report.fail("box remove", err.to_string()),
    }

    // --- 10. shutdown ---
    match runtime.shutdown(None).await {
        Ok(()) => report.pass("runtime shutdown", ""),
        Err(err) => report.fail("runtime shutdown", err.to_string()),
    }

    summarize(&report, &home_dir, home_is_temp, keep_home);
}

fn summarize(report: &Report, home_dir: &PathBuf, home_is_temp: bool, keep_home: bool) {
    if home_is_temp && !keep_home {
        let _ = std::fs::remove_dir_all(home_dir);
    } else {
        println!();
        println!("home dir retained at {}", home_dir.display());
    }

    println!();
    println!(
        "{} passed, {} failed",
        report.passed.len(),
        report.failed.len()
    );
    if !report.failed.is_empty() {
        for (step, detail) in &report.failed {
            println!("  FAILED  {step}: {detail}");
        }
        std::process::exit(1);
    }
}
