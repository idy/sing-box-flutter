// Standalone production-policy tests: no Flutter runtime or framework required.
// Run elevated on Windows; inability to create a secure fixture is a failure.
#include "include/sing_box_flutter/secure_core_launch.h"

#include <aclapi.h>
#include <sddl.h>

#include <cstdio>
#include <cstdlib>
#include <memory>
#include <string>
#include <vector>

namespace {

constexpr wchar_t kTrusted[] =
    L"O:BAG:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FRFX;;;BU)";
constexpr wchar_t kHelper[] = L"\\sing_box_flutter_core.exe";

void Require(bool condition, const char* description) {
  if (!condition) {
    std::fprintf(stderr, "FAILED: %s (Win32 %lu)\n", description, GetLastError());
    std::exit(1);
  }
}

struct LocalFreeDeleter {
  void operator()(void* value) const { LocalFree(value); }
};
using Descriptor = std::unique_ptr<void, LocalFreeDeleter>;

Descriptor Parse(const std::wstring& sddl) {
  PSECURITY_DESCRIPTOR descriptor = nullptr;
  Require(ConvertStringSecurityDescriptorToSecurityDescriptorW(
              sddl.c_str(), SDDL_REVISION_1, &descriptor, nullptr) != FALSE,
          "parse security fixture");
  return Descriptor(descriptor);
}

void ApplyDescriptor(const std::wstring& path, const std::wstring& sddl) {
  auto descriptor = Parse(sddl);
  Require(SetFileSecurityW(path.c_str(), OWNER_SECURITY_INFORMATION |
                                            DACL_SECURITY_INFORMATION |
                                            PROTECTED_DACL_SECURITY_INFORMATION,
                           descriptor.get()) != FALSE,
          "apply fixture descriptor");
}

std::wstring CurrentUser() {
  HANDLE token = nullptr;
  Require(OpenProcessToken(GetCurrentProcess(), TOKEN_QUERY, &token) != FALSE,
          "open test token");
  DWORD size = 0;
  GetTokenInformation(token, TokenUser, nullptr, 0, &size);
  std::vector<BYTE> buffer(size);
  Require(GetTokenInformation(token, TokenUser, buffer.data(), size, &size) != FALSE,
          "read test user");
  LPWSTR text = nullptr;
  Require(ConvertSidToStringSidW(reinterpret_cast<TOKEN_USER*>(buffer.data())->User.Sid,
                                 &text) != FALSE,
          "format user SID");
  std::wstring user(text);
  LocalFree(text);
  CloseHandle(token);
  return user;
}

void DescriptorTests() {
  using sing_box_flutter::IsTrustedLaunchDescriptor;
  auto trusted = Parse(kTrusted);
  Require(IsTrustedLaunchDescriptor(trusted.get(), false), "trusted descriptor");
  auto installer = Parse(
      L"O:S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"
      L"D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FRFX;;;BU)");
  Require(IsTrustedLaunchDescriptor(installer.get(), false), "TrustedInstaller owner");
  auto user_owned = Parse(L"O:" + CurrentUser() +
                          L"D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FRFX;;;BU)");
  Require(!IsTrustedLaunchDescriptor(user_owned.get(), false),
          "user owner rejected despite read-only explicit grants");
  auto unknown_owner = Parse(L"O:BUD:P(A;;FA;;;SY)(A;;FA;;;BA)");
  Require(!IsTrustedLaunchDescriptor(unknown_owner.get(), false),
          "untrusted group owner rejected");
  auto null_dacl = Parse(L"O:BAD:NO_ACCESS_CONTROL");
  Require(!IsTrustedLaunchDescriptor(null_dacl.get(), false), "NULL DACL rejected");
  // SDDL DC is directory-service DELETE_CHILD (0x2), which means
  // FILE_ADD_FILE for a filesystem object. Use FILE_DELETE_CHILD's real 0x40.
  for (const auto* rights : {L"GA", L"GW", L"WD", L"WO", L"SD", L"0x00000040",
                              L"0x00000010", L"0x00000100", L"0x01000000"}) {
    auto descriptor = Parse(std::wstring(L"O:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;") +
                            rights + L";;;WD)");
    Require(!IsTrustedLaunchDescriptor(descriptor.get(), false),
            "untrusted mutation grant rejected for executable");
    Require(!IsTrustedLaunchDescriptor(descriptor.get(), true),
            "untrusted mutation grant rejected for ancestor");
  }
  for (const auto* rights : {L"0x00000002", L"0x00000004"}) {
    auto descriptor = Parse(std::wstring(L"O:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;") +
                            rights + L";;;AU)");
    Require(IsTrustedLaunchDescriptor(descriptor.get(), true),
            "ancestor sibling creation accepted");
    Require(!IsTrustedLaunchDescriptor(descriptor.get(), false),
            "app directory child creation and file writes rejected");
  }
  auto inherit_only = Parse(L"O:BAD:P(A;;FA;;;BA)(A;OICIIO;GA;;;CO)");
  Require(IsTrustedLaunchDescriptor(inherit_only.get(), false),
          "inherit-only Creator Owner grant does not affect current object");
  auto owner_rights = Parse(L"O:BAD:P(A;;FA;;;BA)(A;;WD;;;OW)");
  Require(!IsTrustedLaunchDescriptor(owner_rights.get(), false),
          "OWNER RIGHTS broad grant rejected conservatively");
  auto denied_write = Parse(L"O:BAD:P(D;;GW;;;BU)(A;;GA;;;BU)(A;;FA;;;BA)");
  Require(!IsTrustedLaunchDescriptor(denied_write.get(), false),
          "deny entry cannot conceal broad allow");
}

std::wstring TestExecutable() {
  std::vector<wchar_t> buffer(32768);
  const auto size = GetModuleFileNameW(nullptr, buffer.data(),
                                       static_cast<DWORD>(buffer.size()));
  Require(size > 0 && size < buffer.size(), "locate test executable");
  return std::wstring(buffer.data(), size);
}

void MakeDirectory(const std::wstring& path, const std::wstring& sddl = kTrusted) {
  auto descriptor = Parse(sddl);
  SECURITY_ATTRIBUTES attributes{sizeof(SECURITY_ATTRIBUTES), descriptor.get(), FALSE};
  Require(CreateDirectoryW(path.c_str(), &attributes) != FALSE,
          "create new private fixture directory");
}

void CopyHelper(const std::wstring& directory) {
  const auto path = directory + kHelper;
  Require(CopyFileW(TestExecutable().c_str(), path.c_str(), TRUE) != FALSE,
          "create fixture executable");
  // The fixture directory was private from creation. Test copies are explicitly
  // assigned the production deployment policy before testing the launcher.
  ApplyDescriptor(path, kTrusted);
}

void RemoveHelper(const std::wstring& directory) {
  Require(DeleteFileW((directory + kHelper).c_str()) != FALSE,
          "remove fixture executable");
}

void LaunchProbe(const std::wstring& executable) {
  std::wstring command = L"\"" + executable + L"\" --probe";
  STARTUPINFOW startup{};
  startup.cb = sizeof(startup);
  PROCESS_INFORMATION process{};
  Require(CreateProcessW(executable.c_str(), command.data(), nullptr, nullptr,
                          FALSE, CREATE_NO_WINDOW, nullptr, nullptr, &startup,
                          &process) != FALSE,
          "launch the pinned image using volume GUID path");
  Require(WaitForSingleObject(process.hProcess, 15000) == WAIT_OBJECT_0,
          "probe child exits");
  DWORD result = 1;
  Require(GetExitCodeProcess(process.hProcess, &result) && result == 0,
          "probe child executed expected code");
  CloseHandle(process.hThread);
  CloseHandle(process.hProcess);
}

void FilesystemTests() {
  using sing_box_flutter::CoreLaunchLease;
  wchar_t windows[MAX_PATH]{};
  Require(GetWindowsDirectoryW(windows, MAX_PATH) > 2, "locate local system volume");
  // A fresh private directory on the actual volume root avoids silently trusting
  // a user-owned TEMP/checkout ancestor. No existing directory ACL is changed.
  const std::wstring root = std::wstring(windows, 3) + L"SingBoxFlutterLaunchTest-" +
                            std::to_wstring(GetCurrentProcessId()) + L"-" +
                            std::to_wstring(GetTickCount64());
  MakeDirectory(root);
  const auto app = root + L"\\app";
  MakeDirectory(app);
  CopyHelper(app);
  const auto executable = app + kHelper;
  auto lease = CoreLaunchLease::Acquire(executable);
  Require(lease != nullptr, "secure installed helper accepted");
  Require(lease->executable().compare(0, 11, L"\\\\?\\Volume{") == 0,
          "launch path uses stable volume GUID");
  HANDLE writer = CreateFileW(executable.c_str(), FILE_WRITE_DATA,
                              FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE,
                              nullptr, OPEN_EXISTING, 0, nullptr);
  Require(writer == INVALID_HANDLE_VALUE, "pinned helper blocks writes");
  Require(!MoveFileExW(executable.c_str(), (executable + L".moved").c_str(), 0),
          "pinned helper blocks replacement");
  Require(!MoveFileExW(app.c_str(), (app + L".moved").c_str(), 0),
          "immediate directory remains pinned");
  Require(!MoveFileExW(root.c_str(), (root + L".moved").c_str(), 0),
          "ancestor directory remains pinned");
  writer = CreateFileW(app.c_str(), FILE_WRITE_DATA,
                       FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE,
                       nullptr, OPEN_EXISTING, FILE_FLAG_BACKUP_SEMANTICS, nullptr);
  Require(writer == INVALID_HANDLE_VALUE, "pinned directory blocks reparse mutation");
  LaunchProbe(lease->executable());
  lease.reset();
  writer = CreateFileW(executable.c_str(), FILE_WRITE_DATA,
                       FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE,
                       nullptr, OPEN_EXISTING, 0, nullptr);
  Require(writer != INVALID_HANDLE_VALUE, "release removes write-sharing lease");
  Require(CoreLaunchLease::Acquire(executable) == nullptr,
          "already-open write handle prevents authorization");
  CloseHandle(writer);
  writer = CreateFileW(app.c_str(), DELETE,
                       FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE,
                       nullptr, OPEN_EXISTING, FILE_FLAG_BACKUP_SEMANTICS, nullptr);
  Require(writer != INVALID_HANDLE_VALUE, "open retained directory delete handle");
  Require(CoreLaunchLease::Acquire(executable) == nullptr,
          "already-open ancestor delete handle prevents authorization");
  CloseHandle(writer);

  const auto hardlink = app + L"\\alias.exe";
  Require(CreateHardLinkW(hardlink.c_str(), executable.c_str(), nullptr) != FALSE,
          "create hard-link fixture");
  Require(CoreLaunchLease::Acquire(executable) == nullptr, "multi-link helper rejected");
  Require(DeleteFileW(hardlink.c_str()) != FALSE, "remove hard-link fixture");

  const auto user = CurrentUser();
  ApplyDescriptor(executable, L"O:" + user +
                                  L"D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FRFX;;;BU)");
  Require(CoreLaunchLease::Acquire(executable) == nullptr, "user-owned helper rejected");
  ApplyDescriptor(executable, kTrusted);
  ApplyDescriptor(executable, L"O:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;GW;;;BU)");
  Require(CoreLaunchLease::Acquire(executable) == nullptr,
          "medium-writable helper rejected before execution");
  ApplyDescriptor(executable, kTrusted);
  ApplyDescriptor(app, L"O:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x00000040;;;BU)");
  Require(CoreLaunchLease::Acquire(executable) == nullptr,
          "ancestor delete-child grant rejected");
  ApplyDescriptor(app, kTrusted);
  ApplyDescriptor(root, L"O:" + user + L"D:P(A;;FA;;;SY)(A;;FA;;;BA)");
  Require(CoreLaunchLease::Acquire(executable) == nullptr,
          "user-owned ancestor rejected even with private helper ACL");
  ApplyDescriptor(root, kTrusted);

  const auto link = root + L"\\link";
  Require(CreateSymbolicLinkW(link.c_str(), app.c_str(), SYMBOLIC_LINK_FLAG_DIRECTORY) != 0,
          "create directory reparse fixture");
  Require(CoreLaunchLease::Acquire(link + kHelper) == nullptr,
          "reparse-point ancestor rejected");
  Require(RemoveDirectoryW(link.c_str()) != FALSE, "remove directory reparse fixture");
  const auto saved = app + L"\\saved.exe";
  Require(MoveFileW(executable.c_str(), saved.c_str()) != FALSE,
          "move helper to create file reparse fixture");
  Require(CreateSymbolicLinkW(executable.c_str(), saved.c_str(), 0) != 0,
          "create file reparse fixture");
  Require(CoreLaunchLease::Acquire(executable) == nullptr, "reparse-point helper rejected");
  Require(DeleteFileW(executable.c_str()) != FALSE, "remove file reparse fixture");
  Require(MoveFileW(saved.c_str(), executable.c_str()) != FALSE, "restore fixture helper");

  // DOS aliases must not omit a user-controlled directory from verification.
  wchar_t unused_drive[] = L"Z:";
  bool mapped = false;
  for (wchar_t drive = L'Z'; drive >= L'D'; --drive) {
    unused_drive[0] = drive;
    const std::wstring drive_root = std::wstring(unused_drive) + L"\\";
    if (GetDriveTypeW(drive_root.c_str()) != DRIVE_NO_ROOT_DIR) continue;
    Require(DefineDosDeviceW(0, unused_drive, root.c_str()) != FALSE,
            "create SUBST-style DOS alias fixture");
    Require(CoreLaunchLease::Acquire(std::wstring(unused_drive) + L"\\app" + kHelper) == nullptr,
            "DOS alias hiding ancestors rejected");
    Require(DefineDosDeviceW(DDD_REMOVE_DEFINITION | DDD_EXACT_MATCH_ON_REMOVE,
                             unused_drive, root.c_str()) != FALSE,
            "remove DOS alias fixture");
    mapped = true;
    break;
  }
  Require(mapped, "available drive letter for alias regression");

  for (const auto& invalid : {
           app + L"\\..\\app" + kHelper, app + L"\\.\\" + kHelper,
           executable + L":stream", executable + L".",
           L"\\\\?\\" + executable, L"\\\\localhost\\C$\\missing" +
                                               std::wstring(kHelper)}) {
    Require(CoreLaunchLease::Acquire(invalid) == nullptr,
            "ambiguous or remote path rejected");
  }
  Require(CoreLaunchLease::Acquire(executable) != nullptr,
          "failed authorizations release every acquired handle");
  RemoveHelper(app);
  Require(RemoveDirectoryW(app.c_str()) != FALSE, "remove fixture app directory");
  Require(RemoveDirectoryW(root.c_str()) != FALSE, "remove fixture root directory");
}

}  // namespace

int wmain(int argc, wchar_t** argv) {
  if (argc == 2 && std::wstring(argv[1]) == L"--probe") return 0;
  DescriptorTests();
  FilesystemTests();
  std::puts("Secure Windows core launch policy tests passed.");
  return 0;
}
