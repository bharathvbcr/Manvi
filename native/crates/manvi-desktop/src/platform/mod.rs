//! Native process lifetime boundary. Broker policy and state transitions stay
//! in safe Rust; OS handles, jobs and post-fork descriptor flags live here.
use desktop_core::{DesktopError, Result};
use std::process::{Child, Command};
use std::thread;

pub(crate) struct SupervisedChild {
    child: Child,
    #[cfg(windows)]
    _job: std::os::windows::io::OwnedHandle,
}
impl SupervisedChild {
    pub(crate) fn new(child: Child) -> Result<Self> {
        #[cfg(windows)]
        {
            use std::os::windows::io::{AsRawHandle, FromRawHandle, OwnedHandle};
            use windows::{
                Win32::{
                    Foundation::HANDLE,
                    System::JobObjects::{
                        AssignProcessToJobObject, CreateJobObjectW,
                        JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, JOBOBJECT_EXTENDED_LIMIT_INFORMATION,
                        JobObjectExtendedLimitInformation, SetInformationJobObject,
                    },
                },
                core::PCWSTR,
            };
            let setup = (|| -> windows::core::Result<OwnedHandle> {
                let raw = unsafe { CreateJobObjectW(None, PCWSTR::null()) }?;
                let owned = unsafe { OwnedHandle::from_raw_handle(raw.0) };
                let mut limits = JOBOBJECT_EXTENDED_LIMIT_INFORMATION::default();
                limits.BasicLimitInformation.LimitFlags = JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE;
                unsafe {
                    SetInformationJobObject(
                        raw,
                        JobObjectExtendedLimitInformation,
                        (&limits as *const JOBOBJECT_EXTENDED_LIMIT_INFORMATION).cast(),
                        std::mem::size_of_val(&limits) as u32,
                    )
                }?;
                unsafe { AssignProcessToJobObject(raw, HANDLE(child.as_raw_handle())) }?;
                Ok(owned)
            })();
            match setup {
                Ok(job) => Ok(Self { child, _job: job }),
                Err(e) => {
                    let mut child = child;
                    terminate(&mut child)?;
                    Err(DesktopError::new("helper_supervision", e.to_string()))
                }
            }
        }
        #[cfg(not(windows))]
        {
            Ok(Self { child })
        }
    }
}
impl std::ops::Deref for SupervisedChild {
    type Target = Child;
    fn deref(&self) -> &Child {
        &self.child
    }
}
impl std::ops::DerefMut for SupervisedChild {
    fn deref_mut(&mut self) -> &mut Child {
        &mut self.child
    }
}
impl Drop for SupervisedChild {
    fn drop(&mut self) {
        if let Err(e) = terminate(&mut self.child) {
            eprintln!("native helper cleanup: {e}");
        }
    }
}
pub(crate) fn watch_parent(pid: u32) -> Result<()> {
    #[cfg(unix)]
    {
        if unsafe { libc::getppid() } != pid as i32 {
            return Err(DesktopError::new(
                "parent_missing",
                "Native supervisor already exited",
            ));
        }
        thread::spawn(move || {
            loop {
                if unsafe { libc::getppid() } != pid as i32 {
                    std::process::exit(125);
                }
                thread::sleep(std::time::Duration::from_millis(25));
            }
        });
    }
    #[cfg(windows)]
    {
        use std::os::windows::io::{AsRawHandle, FromRawHandle, OwnedHandle};
        use windows::Win32::{
            Foundation::HANDLE,
            System::Threading::{INFINITE, OpenProcess, PROCESS_SYNCHRONIZE, WaitForSingleObject},
        };
        let handle = unsafe { OpenProcess(PROCESS_SYNCHRONIZE, false, pid) }
            .map_err(|e| DesktopError::new("parent_missing", e.to_string()))?;
        let owned = unsafe { OwnedHandle::from_raw_handle(handle.0) };
        thread::spawn(move || {
            unsafe { WaitForSingleObject(HANDLE(owned.as_raw_handle()), INFINITE) };
            std::process::exit(125);
        });
    }
    Ok(())
}

/// Handle the normal exit/kill race without turning an already exited helper
/// into an unsuccessful pause. No native call runs on this broker thread.
pub(crate) fn terminate(child: &mut Child) -> Result<()> {
    if child
        .try_wait()
        .map_err(|e| DesktopError::new("helper_reap", e.to_string()))?
        .is_none()
        && let Err(kill_error) = child.kill()
        && child
            .try_wait()
            .map_err(|e| DesktopError::new("helper_reap", e.to_string()))?
            .is_none()
    {
        return Err(DesktopError::new("helper_cancel", kill_error.to_string()).uncertain());
    }
    child
        .wait()
        .map_err(|e| DesktopError::new("helper_reap", e.to_string()))?;
    Ok(())
}

pub(crate) fn inherit_lease(command: &mut Command, lease: Option<&std::fs::File>) {
    #[cfg(unix)]
    if let Some(file) = lease {
        use std::os::{fd::AsRawFd, unix::process::CommandExt};
        let fd = file.as_raw_fd();
        // fcntl is async-signal-safe. Change only the forked child's descriptor,
        // retaining CLOEXEC in the broker so independent probes inherit no lease.
        unsafe {
            command.pre_exec(move || {
                let flags = libc::fcntl(fd, libc::F_GETFD);
                if flags < 0 || libc::fcntl(fd, libc::F_SETFD, flags & !libc::FD_CLOEXEC) < 0 {
                    return Err(std::io::Error::last_os_error());
                }
                Ok(())
            });
        }
    }
    #[cfg(not(unix))]
    let _ = (command, lease);
}

#[cfg(test)]
pub(crate) static CHILD_FIXTURE_LOCK: std::sync::Mutex<()> = std::sync::Mutex::new(());

#[cfg(all(test, unix))]
mod tests {
    use super::{CHILD_FIXTURE_LOCK, SupervisedChild, inherit_lease};
    use desktop_core::now_ms;
    use std::process::{Command, Stdio};
    #[cfg(unix)]
    #[test]
    fn inherited_kernel_lease_outlives_supervisor_file_handle() {
        let _fixture_guard = CHILD_FIXTURE_LOCK.lock().unwrap();
        use std::os::fd::AsRawFd;
        let path = std::env::temp_dir().join(format!(
            "manvi-lease-test-{}-{}",
            std::process::id(),
            now_ms()
        ));
        let lease = std::fs::File::create_new(&path).unwrap();
        lease.try_lock().unwrap();
        let fd = lease.as_raw_fd();
        let flags = unsafe { libc::fcntl(fd, libc::F_GETFD) };
        assert!(flags & libc::FD_CLOEXEC != 0);
        let mut command = Command::new(std::env::current_exe().unwrap());
        command
            .args([
                "--exact",
                "broker::tests::helper_fixture_blocks_until_parent_closes_pipe",
            ])
            .env("MANVI_TEST_BLOCK_HELPER", "1")
            .stdin(Stdio::piped())
            .stdout(Stdio::null());
        inherit_lease(&mut command, Some(&lease));
        let child = command.spawn().unwrap();
        assert!(unsafe { libc::fcntl(fd, libc::F_GETFD) } & libc::FD_CLOEXEC != 0);
        let child = SupervisedChild::new(child).unwrap();
        drop(lease);
        let other = std::fs::OpenOptions::new()
            .read(true)
            .write(true)
            .open(&path)
            .unwrap();
        assert!(other.try_lock().is_err());
        drop(child);
        other.try_lock().unwrap();
        drop(other);
        std::fs::remove_file(path).unwrap();
    }
}
