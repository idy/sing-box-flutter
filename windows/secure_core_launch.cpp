#include "include/sing_box_flutter/secure_core_launch.h"

#include <aclapi.h>
#include <winternl.h>

#include <array>
#include <cstddef>
#include <cstring>
#include <limits>
#include <utility>

namespace sing_box_flutter {
namespace {

constexpr wchar_t kHelperName[] = L"sing_box_flutter_core.exe";
constexpr ULONG kDontReparse = 0x00001000;
constexpr ULONG kOpenReparsePoint = 0x00200000;
constexpr ULONG kDirectoryFile = 0x00000001;
constexpr ULONG kNonDirectoryFile = 0x00000040;
constexpr ULONG kSynchronousIoNonalert = 0x00000020;
constexpr ULONG kFileOpen = 1;
using NtOpen = NTSTATUS(NTAPI*)(PHANDLE, ACCESS_MASK, POBJECT_ATTRIBUTES,
                               PIO_STATUS_BLOCK, PLARGE_INTEGER, ULONG, ULONG,
                               ULONG, ULONG, PVOID, ULONG);
using NtOpenLink = NTSTATUS(NTAPI*)(PHANDLE, ACCESS_MASK, POBJECT_ATTRIBUTES);
using NtQueryLink = NTSTATUS(NTAPI*)(HANDLE, PUNICODE_STRING, PULONG);
using NtQueryName = NTSTATUS(NTAPI*)(HANDLE, OBJECT_INFORMATION_CLASS, PVOID,
                                    ULONG, PULONG);
constexpr ACCESS_MASK kLinkQuery = 1;
constexpr ACCESS_MASK kLinkSet = 2;
constexpr ACCESS_MASK kDirectoryQuery = 1;

template <typename Function>
Function NativeFunction(HMODULE module, const char* name) {
  const auto address = GetProcAddress(module, name);
  Function function = nullptr;
  static_assert(sizeof(function) == sizeof(address), "Windows function pointer size");
  std::memcpy(&function, &address, sizeof(function));
  return function;
}

bool TrustedDescriptor(PSECURITY_DESCRIPTOR descriptor, GENERIC_MAPPING mapping,
                       DWORD harmless);

struct LocalFreeDeleter {
  void operator()(void* value) const { LocalFree(value); }
};
using LocalMemory = std::unique_ptr<void, LocalFreeDeleter>;

bool TrustedSid(PSID sid) {
  // These group/service SIDs cannot be enabled by an ordinary medium token.
  // In particular the current user's SID is NOT a trusted owner, even when the
  // calling process has that user's elevated token: owners have implicit DAC.
  if (!sid || !IsValidSid(sid)) return false;
  if (IsWellKnownSid(sid, WinLocalSystemSid) ||
      IsWellKnownSid(sid, WinBuiltinAdministratorsSid)) {
    return true;
  }
  SID_IDENTIFIER_AUTHORITY authority = SECURITY_NT_AUTHORITY;
  PSID installer = nullptr;
  if (!AllocateAndInitializeSid(&authority, 6, SECURITY_SERVICE_ID_BASE_RID,
                               956008885, 3418522649, 1831038044, 1853292631,
                               2271478464, 0, 0, &installer)) {
    return false;
  }
  const bool trusted = EqualSid(sid, installer) != FALSE;
  FreeSid(installer);
  return trusted;
}

bool LiteralComponent(const std::wstring& name) {
  if (name.empty() || name == L"." || name == L".." || name.back() == L'.' ||
      name.back() == L' ') {
    return false;
  }
  for (const auto character : name) {
    if (character < 32 || std::wstring(L"\\/:*?\"<>|").find(character) !=
                              std::wstring::npos) {
      return false;
    }
  }
  const auto stem = name.substr(0, name.find(L'.'));
  for (const auto* reserved : {L"CON", L"PRN", L"AUX", L"NUL", L"CONIN$",
                               L"CONOUT$"}) {
    if (_wcsicmp(stem.c_str(), reserved) == 0) return false;
  }
  if (stem.size() == 4 &&
      (_wcsnicmp(stem.c_str(), L"COM", 3) == 0 ||
       _wcsnicmp(stem.c_str(), L"LPT", 3) == 0) &&
      ((stem[3] >= L'1' && stem[3] <= L'9') || stem[3] == 0x00b9 ||
       stem[3] == 0x00b2 || stem[3] == 0x00b3)) {
    return false;
  }
  return true;
}

HANDLE OpenPinned(NtOpen open, HANDLE parent, const std::wstring& name,
                  bool directory) {
  if (name.size() > (std::numeric_limits<USHORT>::max)() / sizeof(wchar_t)) {
    return INVALID_HANDLE_VALUE;
  }
  UNICODE_STRING object_name{};
  object_name.Buffer = const_cast<PWSTR>(name.data());
  object_name.Length = static_cast<USHORT>(name.size() * sizeof(wchar_t));
  object_name.MaximumLength = object_name.Length;
  OBJECT_ATTRIBUTES attributes{};
  attributes.Length = sizeof(attributes);
  attributes.RootDirectory = parent;
  attributes.ObjectName = &object_name;
  // Honor the verified parent's case-sensitivity policy, including distinct
  // sibling names on NTFS, without rejecting normal case-insensitive host paths.
  DWORD case_flags = 0;
  if (parent &&
      !GetFileInformationByHandleEx(
          parent, static_cast<FILE_INFO_BY_HANDLE_CLASS>(23), &case_flags,
          sizeof(case_flags))) {
    const auto error = GetLastError();
    if (error != ERROR_INVALID_PARAMETER && error != ERROR_NOT_SUPPORTED &&
        error != ERROR_INVALID_FUNCTION) {
      return INVALID_HANDLE_VALUE;
    }
    // Older Windows/filesystems cannot enable directory case sensitivity.
    case_flags = 0;
  }
  if ((case_flags & ~DWORD{1}) != 0) return INVALID_HANDLE_VALUE;
  attributes.Attributes =
      kDontReparse | ((case_flags & 1) ? 0 : OBJ_CASE_INSENSITIVE);
  HANDLE handle = INVALID_HANDLE_VALUE;
  IO_STATUS_BLOCK status{};
  const auto result = open(
      &handle, READ_CONTROL | FILE_READ_ATTRIBUTES | SYNCHRONIZE |
                   (directory ? FILE_LIST_DIRECTORY | FILE_TRAVERSE
                              : FILE_READ_DATA | FILE_EXECUTE),
      &attributes, &status, nullptr, FILE_ATTRIBUTE_NORMAL, FILE_SHARE_READ,
      kFileOpen, kOpenReparsePoint | kSynchronousIoNonalert |
                     (directory ? kDirectoryFile : kNonDirectoryFile),
      nullptr, 0);
  return result < 0 ? INVALID_HANDLE_VALUE : handle;
}

bool VerifyPinned(HANDLE handle, bool directory, bool allow_sibling_creation) {
  BY_HANDLE_FILE_INFORMATION information{};
  if (!GetFileInformationByHandle(handle, &information) ||
      (information.dwFileAttributes & FILE_ATTRIBUTE_REPARSE_POINT) != 0 ||
      ((information.dwFileAttributes & FILE_ATTRIBUTE_DIRECTORY) != 0) !=
          directory ||
      (!directory && information.nNumberOfLinks != 1) ||
      (information.nFileIndexHigh == 0 && information.nFileIndexLow == 0)) {
    return false;
  }
  PSECURITY_DESCRIPTOR descriptor = nullptr;
  const auto result = GetSecurityInfo(handle, SE_FILE_OBJECT,
                                      OWNER_SECURITY_INFORMATION |
                                          DACL_SECURITY_INFORMATION,
                                      nullptr, nullptr, nullptr, nullptr,
                                      &descriptor);
  LocalMemory memory(descriptor);
  return result == ERROR_SUCCESS &&
         IsTrustedLaunchDescriptor(descriptor, allow_sibling_creation);
}

std::wstring ObjectName(NtQueryName query, HANDLE handle) {
  std::vector<BYTE> buffer(65536);
  if (query(handle, static_cast<OBJECT_INFORMATION_CLASS>(1), buffer.data(),
            static_cast<ULONG>(buffer.size()), nullptr) < 0) {
    return {};
  }
  const auto* name = reinterpret_cast<const UNICODE_STRING*>(buffer.data());
  if (!name->Buffer || !name->Length || name->Length % sizeof(wchar_t) != 0) {
    return {};
  }
  return std::wstring(name->Buffer, name->Length / sizeof(wchar_t));
}

bool PinNamespaceLink(NtOpenLink open, NtQueryLink query,
                      const std::wstring& name, const std::wstring& expected,
                      std::vector<HANDLE>& handles) {
  UNICODE_STRING object_name{};
  object_name.Buffer = const_cast<PWSTR>(name.data());
  object_name.Length = static_cast<USHORT>(name.size() * sizeof(wchar_t));
  object_name.MaximumLength = object_name.Length;
  OBJECT_ATTRIBUTES attributes{};
  attributes.Length = sizeof(attributes);
  attributes.ObjectName = &object_name;
  attributes.Attributes = OBJ_CASE_INSENSITIVE;
  HANDLE handle = nullptr;
  if (open(&handle, READ_CONTROL | kLinkQuery, &attributes) < 0) return false;
  handles.push_back(handle);

  std::array<wchar_t, 64> buffer{};
  UNICODE_STRING target{};
  target.Buffer = buffer.data();
  target.MaximumLength = static_cast<USHORT>(sizeof(buffer));
  if (query(handle, &target, nullptr) < 0 ||
      target.Length != expected.size() * sizeof(wchar_t) ||
      _wcsnicmp(buffer.data(), expected.data(), expected.size()) != 0) {
    return false;
  }
  PSECURITY_DESCRIPTOR descriptor = nullptr;
  const auto result = GetSecurityInfo(handle, SE_KERNEL_OBJECT,
                                      OWNER_SECURITY_INFORMATION |
                                          DACL_SECURITY_INFORMATION,
                                      nullptr, nullptr, nullptr, nullptr,
                                      &descriptor);
  LocalMemory memory(descriptor);
  // Symbolic-link targets are immutable through a query handle. A replacement
  // requires DELETE; changing the descriptor requires WRITE_DAC/WRITE_OWNER.
  // Only trusted owners/grants may have those rights (or the link's SET right).
  GENERIC_MAPPING mapping{STANDARD_RIGHTS_READ | kLinkQuery,
                          STANDARD_RIGHTS_WRITE | kLinkSet,
                          STANDARD_RIGHTS_EXECUTE | kLinkQuery,
                          STANDARD_RIGHTS_REQUIRED | kLinkQuery | kLinkSet};
  return result == ERROR_SUCCESS &&
         TrustedDescriptor(descriptor, mapping, READ_CONTROL | kLinkQuery);
}

std::wstring VolumeRoot(HANDLE handle) {
  // Reject UNC, mapped network drives and SUBST aliases (which hide ancestors).
  // A GUID is useful for verifying the root, but CreateProcessW rejects the
  // extended \\?\Volume{GUID} image form with ERROR_INVALID_PARAMETER.
  std::array<wchar_t, 64> name{};
  const DWORD size = GetFinalPathNameByHandleW(
      handle, name.data(), static_cast<DWORD>(name.size()), VOLUME_NAME_GUID);
  if (size != 49 || size >= name.size()) return {};
  const std::wstring root(name.data(), size);
  if (root.compare(0, 11, L"\\\\?\\Volume{") != 0 ||
      root.compare(47, 2, L"}\\") != 0) {
    return {};
  }
  // A volume GUID contains exactly 8-4-4-4-12 hexadecimal digits.
  for (size_t index = 11; index < 47; ++index) {
    const auto value = root[index];
    if (index == 19 || index == 24 || index == 29 || index == 34) {
      if (value != L'-') return {};
    } else if (!((value >= L'0' && value <= L'9') ||
                 (value >= L'a' && value <= L'f') ||
                 (value >= L'A' && value <= L'F'))) {
      return {};
    }
  }
  DWORD flags = 0;
  if (!GetVolumeInformationByHandleW(handle, nullptr, 0, nullptr, nullptr,
                                     &flags, nullptr, 0) ||
      !(flags & FILE_PERSISTENT_ACLS) ||
      GetDriveTypeW(root.c_str()) != DRIVE_FIXED) {
    return {};
  }
  // Get the actual device from the pinned root, never QueryDosDevice on the
  // original drive letter. GLOBALROOT reaches the NT object-manager root; Global
  // first bypasses a writable local GLOBALROOT alias. Both links are checked and
  // retained by Acquire. The Win32 device (\\.\) spelling uses normal Win32 path
  // processing rather than assuming CreateProcess accepts every extended file-I/O
  // namespace. Its image and current-directory compatibility is tested on Windows.
  std::array<wchar_t, 128> device{};
  const DWORD length = GetFinalPathNameByHandleW(
      handle, device.data(), static_cast<DWORD>(device.size()), VOLUME_NAME_NT);
  if (!length || length >= device.size()) return {};
  const std::wstring native(device.data(), length);
  const std::wstring prefix = L"\\Device\\HarddiskVolume";
  if (native.compare(0, prefix.size(), prefix) != 0 ||
      native.size() <= prefix.size() + 1 || native.back() != L'\\') {
    return {};
  }
  for (size_t index = prefix.size(); index + 1 < native.size(); ++index) {
    if (native[index] < L'0' || native[index] > L'9') return {};
  }
  return L"\\\\.\\Global\\GLOBALROOT" + native;
}

bool TrustedDescriptor(PSECURITY_DESCRIPTOR descriptor, GENERIC_MAPPING mapping,
                       DWORD harmless) {
  if (!descriptor || !IsValidSecurityDescriptor(descriptor)) return false;
  PSID owner = nullptr;
  BOOL defaulted = FALSE;
  if (!GetSecurityDescriptorOwner(descriptor, &owner, &defaulted) ||
      !TrustedSid(owner)) {
    return false;
  }
  PACL dacl = nullptr;
  BOOL present = FALSE;
  if (!GetSecurityDescriptorDacl(descriptor, &present, &dacl, &defaulted) ||
      !present || !dacl || !IsValidAcl(dacl)) {
    return false;
  }
  for (DWORD index = 0; index < dacl->AceCount; ++index) {
    void* raw_ace = nullptr;
    if (!GetAce(dacl, index, &raw_ace)) return false;
    const auto* header = static_cast<ACE_HEADER*>(raw_ace);
    if (header->AceFlags & INHERIT_ONLY_ACE) continue;
    // Unsupported object/callback/conditional grants fail closed. Ignoring deny
    // entries is conservative: a broad allow is rejected even if a deny happens
    // to mask it for some tokens today.
    if (header->AceType == ACCESS_DENIED_ACE_TYPE) continue;
    if (header->AceType != ACCESS_ALLOWED_ACE_TYPE ||
        header->AceSize < offsetof(ACCESS_ALLOWED_ACE, SidStart) +
                              sizeof(SID) - sizeof(DWORD)) {
      return false;
    }
    const auto* ace = static_cast<ACCESS_ALLOWED_ACE*>(raw_ace);
    auto* sid = const_cast<DWORD*>(&ace->SidStart);
    if (!IsValidSid(sid) ||
        GetLengthSid(sid) >
            header->AceSize - offsetof(ACCESS_ALLOWED_ACE, SidStart)) {
      return false;
    }
    DWORD mask = ace->Mask;
    MapGenericMask(&mask, &mapping);
    if ((mask & ~harmless) != 0 && !TrustedSid(sid)) return false;
  }
  return true;
}

}  // namespace

bool IsTrustedLaunchDescriptor(PSECURITY_DESCRIPTOR descriptor,
                               bool allow_sibling_creation) {
  GENERIC_MAPPING mapping{FILE_GENERIC_READ, FILE_GENERIC_WRITE,
                          FILE_GENERIC_EXECUTE, FILE_ALL_ACCESS};
  DWORD harmless = FILE_GENERIC_READ | FILE_GENERIC_EXECUTE;
  if (allow_sibling_creation) harmless |= FILE_ADD_FILE | FILE_ADD_SUBDIRECTORY;
  return TrustedDescriptor(descriptor, mapping, harmless);
}

CoreLaunchLease::~CoreLaunchLease() {
  for (auto handle = handles_.rbegin(); handle != handles_.rend(); ++handle) {
    CloseHandle(*handle);
  }
}

std::unique_ptr<CoreLaunchLease> CoreLaunchLease::AcquireForHost() {
  std::vector<wchar_t> path(32768);
  const DWORD size = GetModuleFileNameW(nullptr, path.data(),
                                       static_cast<DWORD>(path.size()));
  if (!size || size >= path.size()) return nullptr;
  const std::wstring host(path.data(), size);
  const auto separator = host.find_last_of(L'\\');
  if (separator == std::wstring::npos) return nullptr;
  auto lease = Acquire(host.substr(0, separator + 1) + kHelperName);
  if (!lease) return nullptr;
  // Bind the source drive to the already-running host image as well. A drive
  // remap before acquisition must not select another otherwise trusted volume.
  DWORD native_size = static_cast<DWORD>(path.size());
  if (!QueryFullProcessImageNameW(GetCurrentProcess(), PROCESS_NAME_NATIVE,
                                  path.data(), &native_size)) {
    return nullptr;
  }
  const std::wstring native_host(path.data(), native_size);
  const std::wstring prefix = L"\\Device\\HarddiskVolume";
  const auto root_end = native_host.find(L'\\', prefix.size());
  if (native_host.compare(0, prefix.size(), prefix) != 0 ||
      root_end == std::wstring::npos) {
    return nullptr;
  }
  const auto actual_root = L"\\\\.\\Global\\GLOBALROOT" + native_host.substr(0, root_end + 1);
  if (lease->executable_.size() < actual_root.size() ||
      _wcsnicmp(lease->executable_.c_str(), actual_root.c_str(), actual_root.size()) != 0) {
    return nullptr;
  }
  return lease;
}

std::unique_ptr<CoreLaunchLease> CoreLaunchLease::Acquire(
    const std::wstring& path) {
  // Do not normalize aliases: reject them. Only a local drive-absolute path is
  // accepted as input; the output names the pinned device in the global namespace.
  if (path.size() < 4 ||
      !((path[0] >= L'A' && path[0] <= L'Z') ||
        (path[0] >= L'a' && path[0] <= L'z')) ||
      path[1] != L':' || path[2] != L'\\') {
    return nullptr;
  }
  std::vector<std::wstring> parts;
  for (size_t position = 3; position <= path.size();) {
    const auto next = path.find(L'\\', position);
    const auto part = path.substr(position, next == std::wstring::npos
                                               ? std::wstring::npos
                                               : next - position);
    if (!LiteralComponent(part)) return nullptr;
    parts.push_back(part);
    if (next == std::wstring::npos) break;
    position = next + 1;
  }
  if (parts.size() < 2 || parts.back() != kHelperName) return nullptr;
  const auto module = GetModuleHandleW(L"ntdll.dll");
  if (!module) return nullptr;
  const auto open = NativeFunction<NtOpen>(module, "NtCreateFile");
  const auto open_link =
      NativeFunction<NtOpenLink>(module, "NtOpenSymbolicLinkObject");
  const auto query_link =
      NativeFunction<NtQueryLink>(module, "NtQuerySymbolicLinkObject");
  const auto open_directory =
      NativeFunction<NtOpenLink>(module, "NtOpenDirectoryObject");
  const auto query_name = NativeFunction<NtQueryName>(module, "NtQueryObject");
  if (!open || !open_link || !query_link || !open_directory || !query_name) {
    return nullptr;
  }
  auto lease = std::unique_ptr<CoreLaunchLease>(new CoreLaunchLease());
  wchar_t map_path[] = L"\\??";
  UNICODE_STRING map_name{};
  map_name.Buffer = map_path;
  map_name.Length = static_cast<USHORT>(sizeof(map_path) - sizeof(wchar_t));
  map_name.MaximumLength = static_cast<USHORT>(sizeof(map_path));
  OBJECT_ATTRIBUTES map_attributes{};
  map_attributes.Length = sizeof(map_attributes);
  map_attributes.ObjectName = &map_name;
  map_attributes.Attributes = OBJ_CASE_INSENSITIVE;
  HANDLE device_map = nullptr;
  if (open_directory(&device_map, kDirectoryQuery, &map_attributes) < 0) {
    return nullptr;
  }
  lease->handles_.push_back(device_map);
  const auto map = ObjectName(query_name, device_map);
  // Checking a drive symlink alone is insufficient: a new local alias can shadow
  // a pinned global C: link. Global must be an existing protected entry in this
  // exact device directory, not global fallback that a new local Global can hide.
  // Object directories have no filesystem-style DELETE_CHILD right: their owner
  // cannot replace an existing protected link without DELETE on that link.
  if (map.empty() ||
      !PinNamespaceLink(open_link, query_link, L"\\??\\Global", L"\\GLOBAL??",
                        lease->handles_) ||
      _wcsicmp(ObjectName(query_name, lease->handles_.back()).c_str(),
               (map + L"\\Global").c_str()) != 0 ||
      !PinNamespaceLink(open_link, query_link, L"\\GLOBAL??\\GLOBALROOT", L"",
                        lease->handles_)) {
    return nullptr;
  }
  HANDLE current = OpenPinned(open, nullptr, L"\\??\\" + path.substr(0, 3), true);
  if (current == INVALID_HANDLE_VALUE) return nullptr;
  lease->handles_.push_back(current);
  if (!VerifyPinned(current, true, true)) return nullptr;
  lease->executable_ = VolumeRoot(current);
  if (lease->executable_.empty()) return nullptr;
  for (size_t index = 0; index < parts.size(); ++index) {
    const bool directory = index + 1 < parts.size();
    current = OpenPinned(open, current, parts[index], directory);
    if (current == INVALID_HANDLE_VALUE) return nullptr;
    lease->handles_.push_back(current);
    if (!VerifyPinned(current, directory, index + 2 < parts.size())) {
      return nullptr;
    }
    lease->executable_ += parts[index];
    if (directory) lease->executable_ += L'\\';
  }
  // Dart supplies a quoted command line with a NULL lpApplicationName, for which
  // CreateProcessW documents a MAX_PATH limit on the image name.
  if (lease->executable_.size() >= MAX_PATH) return nullptr;
  return lease;
}

}  // namespace sing_box_flutter
