# Restart implementation audit

This historical audit records the controller before restart coordination was
implemented on November 1, 2026. It describes the earlier behavior and the
requirements that guided the implementation, rather than the current protocol.

* `run` holds crash-released data and account/scale-set locks. Status identifies
  a process with PID, startup timestamp and a random nonce. `Stop` writes that
  nonce into the protected data directory; it never kills a PID.
* The official scaleset v0.5.0 listener polls with total configured capacity,
  calls the serialized `Scale` callback, then acknowledges successful messages.
  `AcquireJobs` is explicit: declining an available request leaves it unassigned.
  The current callback ignores acquisition response IDs and JobAssigned events.
* Reconciliation, acquisition, JIT registration, VM creation and cleanup share
  the controller mutex. The journal precedes remote effects. GitHub's busy-runner
  deletion fence protects executing jobs. Jobs adds observational fields to that
  journal and reads through a separate content-addressed reader executable.
* Shutdown cancels/closes the listener before setting `draining`. It then waits
  only for an empty runner map. Starting/online VMs can be retired immediately
  during draining, without proving that accepted but unstarted work finished.
  This is insufficient as a restart quiescence proof.
* Installed builds use the pinned Go toolchain, source fingerprints, a temporary
  executable and build metadata. Replacing a live executable is prohibited.
  `Activate` calls Stop with a 30-second wait before building; it cannot stage a
  controller update while jobs continue. Other changed runtime inputs cause
  another stop/start in activation's final step.
* Startup records config/PAT/binary/runtime hashes, image identity and effective
  resources. The Hyper-V bridge is snapshotted for the process lifetime. The
  published image has immutable manifests, checksums and provisioning fingerprints.
  CPU/RAM/concurrency do not invalidate the image.
* The existing `CLC-Linux-Runners` task directly runs the installed executable
  with a config path, account, limited token, IgnoreNew and crash recovery. S4U
  adds a boot trigger; Interactive requires the account to remain signed in.
* TestUnattended's fingerprint already excludes ordinary runners/controller
  settings. It includes account SID, boot, session, config path, data/PAT paths,
  complete Hyper-V configuration, image ID, executable and probe scripts. Hardware,
  executable, image and probe changes therefore require revalidation. There is
  no basis to reuse a previous successful probe for these changes automatically.
* Native children normally inherit Windows Job Objects. Detached console flags
  alone do not escape them. Restart must verify successful breakaway before
  permitting the original controller to exit; unsupported hosting must fail
  visibly while preserving the controller and jobs.

Read-only live check: the old controller was listening with two busy runners,
three assigned jobs and unchanged 3-runner / 2-CPU / 10-GiB / 100-GiB settings.
The current terminal lacks Hyper-V inventory permission. No live installation,
configuration change, stop, image build or workload submission is authorized by
this audit's observations as a safe integration test.
