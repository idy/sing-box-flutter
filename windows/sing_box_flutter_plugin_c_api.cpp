#include "include/sing_box_flutter/sing_box_flutter_plugin_c_api.h"

// Control and event delivery use the Dart plugin's anonymous-pipe transport.
// The native target makes Flutter bundle the verified core and Wintun DLL.
void SingBoxFlutterPluginCApiRegisterWithRegistrar(
    FlutterDesktopPluginRegistrarRef /*registrar*/) {}
