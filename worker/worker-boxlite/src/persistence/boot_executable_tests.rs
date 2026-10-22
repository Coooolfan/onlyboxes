use super::super::journal::private_directory;
use super::*;
use std::os::unix::fs::symlink;

struct Root(PathBuf);
impl Root {
    fn new() -> Self {
        let path = PathBuf::from("/tmp").join(format!("obx-bx-{}", uuid::Uuid::new_v4()));
        private_directory(&path).unwrap();
        private_directory(&path.join("boxes")).unwrap();
        fs::copy("/bin/sleep", path.join("fixture-sleep")).unwrap();
        #[cfg(target_os = "macos")]
        assert!(std::process::Command::new("/usr/bin/codesign")
            .args(["--force", "--sign", "-"])
            .arg(path.join("fixture-sleep"))
            .output()
            .unwrap()
            .status
            .success());
        Self(fs::canonicalize(path).unwrap())
    }
    fn prepare(&self, id: &str) -> BootExecutable {
        private_directory(&self.0.join("boxes").join(id)).unwrap();
        BootExecutable::prepare(&self.0, id, &self.0.join("fixture-sleep"), true).unwrap()
    }
}
impl Drop for Root {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.0);
    }
}

#[test]
fn executable_pin_is_private_independent_and_replayable() {
    let root = Root::new();
    let pin = root.prepare("owned");
    let file = File::open(pin.path()).unwrap();
    assert_eq!(file.metadata().unwrap().nlink(), 1);
    assert_eq!(file.metadata().unwrap().permissions().mode() & 0o777, 0o500);
    assert_eq!(
        BootExecutable::prepare(&root.0, "owned", &root.0.join("fixture-sleep"), true).unwrap(),
        pin
    );
    let copy: BootExecutable = serde_json::from_slice(&serde_json::to_vec(&pin).unwrap()).unwrap();
    copy.verify().unwrap();
    assert_eq!(copy, pin);
}

#[test]
fn byte_changes_links_and_directory_replacements_are_not_repaired() {
    let root = Root::new();
    let pin = root.prepare("owned");
    fs::set_permissions(pin.path(), fs::Permissions::from_mode(0o700)).unwrap();
    fs::write(pin.path(), b"different executable").unwrap();
    assert!(pin.verify().is_err());
    assert!(
        BootExecutable::prepare(&root.0, "owned", &root.0.join("fixture-sleep"), true).is_err()
    );
    assert_eq!(fs::read(pin.path()).unwrap(), b"different executable");
    fs::remove_file(pin.path()).unwrap();
    symlink("/bin/sleep", pin.path()).unwrap();
    assert!(
        BootExecutable::prepare(&root.0, "owned", &root.0.join("fixture-sleep"), true).is_err()
    );
    let other = root.prepare("other");
    fs::rename(root.0.join("boxes/other/bin"), root.0.join("saved-bin")).unwrap();
    private_directory(&root.0.join("boxes/other/bin")).unwrap();
    assert!(other.verify().is_err());
}

#[test]
fn unsafe_ids_and_missing_live_executables_do_not_create_replacements() {
    let root = Root::new();
    assert!(
        BootExecutable::prepare(&root.0, "../outside", &root.0.join("fixture-sleep"), true)
            .is_err()
    );
    private_directory(&root.0.join("boxes/owned")).unwrap();
    assert!(
        BootExecutable::prepare(&root.0, "owned", &root.0.join("fixture-sleep"), false).is_err()
    );
    assert!(!root.0.join("boxes/owned/bin").exists());
}

#[cfg(target_os = "macos")]
#[test]
fn kernel_executable_discovery_does_not_need_a_pid_file_or_select_other_boxes() {
    struct Child(std::process::Child);
    impl Drop for Child {
        fn drop(&mut self) {
            let _ = self.0.kill();
            let _ = self.0.wait();
        }
    }
    let root = Root::new();
    let first = root.prepare("first");
    let second = root.prepare("second");
    let mut one = Child(
        std::process::Command::new(first.path())
            .arg("30")
            .spawn()
            .unwrap(),
    );
    let mut two = Child(
        std::process::Command::new(second.path())
            .arg("30")
            .spawn()
            .unwrap(),
    );
    let found = first.discover().unwrap();
    assert_eq!(found.len(), 1);
    assert_eq!(found[0].pid, one.0.id());
    assert!(!root.0.join("boxes/first/shim.pid").exists());
    found[0].request_writer_stop().unwrap();
    one.0.wait().unwrap();
    assert!(two.0.try_wait().unwrap().is_none());
    assert!(found[0].writers_exited().unwrap());
    assert!(first.discover().unwrap().is_empty());
}

#[test]
fn serialized_pin_rejects_foreign_home_and_box_without_requiring_retained_files() {
    let root = Root::new();
    let pin = root.prepare("owned");
    pin.validate_home(&root.0, "owned").unwrap();
    assert_eq!(pin.validate_home(&root.0, "other"), Err(Error::Conflict));
    let foreign = Root::new();
    assert_eq!(pin.validate_home(&foreign.0, "owned"), Err(Error::Conflict));
    fs::remove_dir_all(root.0.join("boxes/owned")).unwrap();
    // Completed operations retain metadata after authorized native deletion.
    pin.validate_home(&root.0, "owned").unwrap();
    assert!(pin.verify().is_err());
}
