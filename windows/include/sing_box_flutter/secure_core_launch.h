#ifndef SING_BOX_FLUTTER_SECURE_CORE_LAUNCH_H_
#define SING_BOX_FLUTTER_SECURE_CORE_LAUNCH_H_

#include <windows.h>

#include <memory>
#include <string>
#include <vector>

namespace sing_box_flutter {

// Read-only, no-write/no-delete-sharing handles pin every path component until
// Process.start completes. Protected Global/GLOBALROOT object-manager links are
// retained too. executable() is a Win32 device path to the pinned NT volume, with
// no DOS drive-letter lookup. Destruction releases the lease, including on failure.
class CoreLaunchLease {
 public:
  ~CoreLaunchLease();
  CoreLaunchLease(const CoreLaunchLease&) = delete;
  CoreLaunchLease& operator=(const CoreLaunchLease&) = delete;

  const std::wstring& executable() const { return executable_; }

  // Uses the running host's directory, never a Dart-supplied production path.
  static std::unique_ptr<CoreLaunchLease> AcquireForHost();

  // Also used by the standalone Windows policy tests. Applies the same policy
  // to an explicit helper path; the method-channel handler does not expose it.
  // Tests may request a fixed rejection-stage label; never includes paths/data.
  static std::unique_ptr<CoreLaunchLease> Acquire(
      const std::wstring& path, const char** rejection_stage = nullptr);

 private:
  CoreLaunchLease() = default;
  std::vector<HANDLE> handles_;
  std::wstring executable_;
};

// Strict descriptor policy shared with native regression tests. Only SYSTEM,
// Administrators and TrustedInstaller may own or mutate executable objects.
// Ancestors may grant creation of siblings: pinned existing entries cannot be
// replaced, and the immediate application directory forbids child creation.
bool IsTrustedLaunchDescriptor(PSECURITY_DESCRIPTOR descriptor,
                               bool allow_sibling_creation);

}  // namespace sing_box_flutter

#endif  // SING_BOX_FLUTTER_SECURE_CORE_LAUNCH_H_
