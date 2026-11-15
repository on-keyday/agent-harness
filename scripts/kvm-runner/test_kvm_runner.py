"""Offline checks for kvm-runner.py's domain definitions (no libvirt needed)."""
import importlib.util
import sys
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
_spec = importlib.util.spec_from_file_location("kvm_runner", HERE / "kvm-runner.py")
kvm = importlib.util.module_from_spec(_spec)
sys.modules["kvm_runner"] = kvm
_spec.loader.exec_module(kvm)


class MemoryBackingTest(unittest.TestCase):
    # virtiofs needs access mode=shared; without source type=memfd libvirt backs
    # the guest's RAM with a file under ~/.config/libvirt/qemu/ram, and every
    # page the guest dirties is written back to disk (a guest wrote 66 TB in
    # 4.5 days that way). Both ways this script defines a domain must say memfd.

    def test_virt_install_args_ask_for_memfd(self):
        self.assertEqual(kvm.MEMORY_BACKING_ARGS,
                         ["--memorybacking", "source.type=memfd,access.mode=shared"])

    def test_attach_adds_memfd_backing_when_none(self):
        xml = "<domain>\n  <currentMemory unit='KiB'>1</currentMemory>\n</domain>\n"
        out = kvm.with_memfd_backing(xml)
        self.assertIn("<source type='memfd'/>", out)
        self.assertIn("<access mode='shared'/>", out)

    def test_attach_adds_memfd_to_a_shared_backing_without_source(self):
        xml = ("<domain>\n  <currentMemory unit='KiB'>1</currentMemory>\n"
               "  <memoryBacking>\n    <access mode='shared'/>\n  </memoryBacking>\n</domain>\n")
        out = kvm.with_memfd_backing(xml)
        self.assertEqual(out.count("<source type='memfd'/>"), 1)
        self.assertEqual(out.count("<memoryBacking>"), 1)

    def test_attach_leaves_an_explicit_source_alone(self):
        xml = ("<domain>\n  <memoryBacking>\n    <source type='memfd'/>\n"
               "    <access mode='shared'/>\n  </memoryBacking>\n</domain>\n")
        self.assertEqual(kvm.with_memfd_backing(xml), xml)


if __name__ == "__main__":
    unittest.main()
