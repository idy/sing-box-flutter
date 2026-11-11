#include "include/sing_box_flutter/sing_box_flutter_plugin_c_api.h"

#include <flutter/method_channel.h>
#include <flutter/plugin_registrar_windows.h>
#include <flutter/standard_method_codec.h>

#include <cstdint>
#include <limits>
#include <map>
#include <memory>
#include <string>

#include "include/sing_box_flutter/secure_core_launch.h"

namespace {

// Configuration/event delivery stays on anonymous pipes. This channel only
// authorizes a bundled executable and pins its complete path through launch.
class SecureCoreLaunchPlugin : public flutter::Plugin {
 public:
  explicit SecureCoreLaunchPlugin(flutter::PluginRegistrarWindows* registrar)
      : channel_(std::make_unique<flutter::MethodChannel<flutter::EncodableValue>>(
            registrar->messenger(), "sing_box_flutter/windows_launch",
            &flutter::StandardMethodCodec::GetInstance())) {
    channel_->SetMethodCallHandler(
        [this](const auto& call, auto result) {
          try {
            Handle(call, result.get());
          } catch (...) {
            // No paths, OS diagnostics or untrusted data cross the boundary.
            result->Error("core_untrusted", "Secure Windows core launch failed.");
          }
        });
  }

  ~SecureCoreLaunchPlugin() override {
    // Engine teardown can race an asynchronous Dart Process.start. With no
    // completion acknowledgement, keep orphaned leases pinned until OS process
    // exit rather than freeing their handles while a child is still launching.
    for (auto& entry : leases_) {
      (void)entry.second.release();
    }
  }

 private:
  void Handle(
      const flutter::MethodCall<flutter::EncodableValue>& call,
      flutter::MethodResult<flutter::EncodableValue>* result) {
    if (call.method_name() == "acquire") {
      // One helper launch can be in flight per engine. A leaked lease remains
      // pinned until process exit and fails subsequent requests closed.
      if (!leases_.empty() || next_ == (std::numeric_limits<int64_t>::max)()) {
        result->Error("core_untrusted", "Secure Windows core launch failed.");
        return;
      }
      auto lease = sing_box_flutter::CoreLaunchLease::AcquireForHost();
      if (!lease) {
        result->Error("core_untrusted", "The Windows core deployment is unsafe.");
        return;
      }
      const auto& path = lease->executable();
      const int size = WideCharToMultiByte(CP_UTF8, WC_ERR_INVALID_CHARS,
                                          path.data(), static_cast<int>(path.size()),
                                          nullptr, 0, nullptr, nullptr);
      if (size <= 0) {
        result->Error("core_untrusted", "Secure Windows core launch failed.");
        return;
      }
      std::string executable(static_cast<size_t>(size), '\0');
      if (WideCharToMultiByte(CP_UTF8, WC_ERR_INVALID_CHARS, path.data(),
                              static_cast<int>(path.size()), executable.data(),
                              size, nullptr, nullptr) != size) {
        result->Error("core_untrusted", "Secure Windows core launch failed.");
        return;
      }
      const int64_t id = ++next_;
      leases_.emplace(id, std::move(lease));
      result->Success(flutter::EncodableValue(flutter::EncodableMap{
          {flutter::EncodableValue("lease"), flutter::EncodableValue(id)},
          {flutter::EncodableValue("executable"),
           flutter::EncodableValue(executable)},
      }));
      return;
    }
    if (call.method_name() == "release") {
      const auto* value = call.arguments();
      int64_t id = 0;
      if (value && std::holds_alternative<int64_t>(*value)) {
        id = std::get<int64_t>(*value);
      } else if (value && std::holds_alternative<int32_t>(*value)) {
        id = std::get<int32_t>(*value);
      }
      if (id <= 0 || leases_.erase(id) != 1) {
        result->Error("core_untrusted", "Secure Windows core launch failed.");
        return;
      }
      result->Success();
      return;
    }
    result->NotImplemented();
  }

  std::unique_ptr<flutter::MethodChannel<flutter::EncodableValue>> channel_;
  std::map<int64_t, std::unique_ptr<sing_box_flutter::CoreLaunchLease>> leases_;
  int64_t next_ = 0;
};

}  // namespace

void SingBoxFlutterPluginCApiRegisterWithRegistrar(
    FlutterDesktopPluginRegistrarRef registrar) {
  auto* windows_registrar = flutter::PluginRegistrarManager::GetInstance()
                                ->GetRegistrar<flutter::PluginRegistrarWindows>(
                                    registrar);
  windows_registrar->AddPlugin(
      std::make_unique<SecureCoreLaunchPlugin>(windows_registrar));
}
