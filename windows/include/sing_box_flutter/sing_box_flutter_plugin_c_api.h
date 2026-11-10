#ifndef FLUTTER_PLUGIN_SING_BOX_FLUTTER_PLUGIN_C_API_H_
#define FLUTTER_PLUGIN_SING_BOX_FLUTTER_PLUGIN_C_API_H_

#include <flutter_plugin_registrar.h>

#ifdef FLUTTER_PLUGIN_IMPL
#define SING_BOX_FLUTTER_EXPORT __declspec(dllexport)
#else
#define SING_BOX_FLUTTER_EXPORT __declspec(dllimport)
#endif

#ifdef __cplusplus
extern "C" {
#endif

SING_BOX_FLUTTER_EXPORT void SingBoxFlutterPluginCApiRegisterWithRegistrar(
    FlutterDesktopPluginRegistrarRef registrar);

#ifdef __cplusplus
}
#endif

#endif  // FLUTTER_PLUGIN_SING_BOX_FLUTTER_PLUGIN_C_API_H_
