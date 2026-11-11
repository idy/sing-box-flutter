cmake_minimum_required(VERSION 3.27)
project(sing_box_flutter LANGUAGES CXX)

set(PLUGIN_NAME "sing_box_flutter_plugin")
add_library(${PLUGIN_NAME} SHARED
  "sing_box_flutter_plugin_c_api.cpp"
  "secure_core_launch.cpp"
  "include/sing_box_flutter/sing_box_flutter_plugin_c_api.h"
)
apply_standard_settings(${PLUGIN_NAME})
set_target_properties(${PLUGIN_NAME} PROPERTIES CXX_VISIBILITY_PRESET hidden)
target_compile_definitions(${PLUGIN_NAME} PRIVATE FLUTTER_PLUGIN_IMPL)
target_include_directories(${PLUGIN_NAME} INTERFACE
  "${CMAKE_CURRENT_SOURCE_DIR}/include")
target_link_libraries(${PLUGIN_NAME} PRIVATE flutter flutter_wrapper_plugin)
if(WIN32)
  target_link_libraries(${PLUGIN_NAME} PRIVATE advapi32)
endif()

# A normal Flutter build validates a previously selected core. It never downloads
# a toolchain or builds Go implicitly, including when the pub cache is shared.
find_package(Python3 3.11 REQUIRED COMPONENTS Interpreter)
# The directory adding the plugin is the Flutter host, even in a superbuild.
get_directory_property(FLUTTER_HOST_DIRECTORY PARENT_DIRECTORY)
if(NOT FLUTTER_HOST_DIRECTORY)
  set(FLUTTER_HOST_DIRECTORY "${CMAKE_CURRENT_SOURCE_DIR}")
endif()
if(NOT FLUTTER_APPLICATION_PATH)
  # generated_config.cmake keeps PROJECT_DIR in Flutter's own subdirectory.
  get_filename_component(FLUTTER_APPLICATION_PATH "${FLUTTER_HOST_DIRECTORY}/.." ABSOLUTE)
endif()
if(NOT EXISTS "${FLUTTER_APPLICATION_PATH}/pubspec.yaml")
  message(FATAL_ERROR "Flutter application path is missing; build from a Flutter Windows host.")
endif()
get_filename_component(PLUGIN_ROOT "${CMAKE_CURRENT_SOURCE_DIR}/.." REALPATH)
if(FLUTTER_TARGET_PLATFORM STREQUAL "windows-arm64")
  set(CORE_ARCH arm64)
elseif(FLUTTER_TARGET_PLATFORM STREQUAL "windows-x64")
  set(CORE_ARCH amd64)
elseif(CMAKE_GENERATOR_PLATFORM MATCHES "^[Aa][Rr][Mm]64($|,)" OR
       CMAKE_VS_PLATFORM_NAME STREQUAL "ARM64")
  set(CORE_ARCH arm64)
elseif(CMAKE_GENERATOR_PLATFORM MATCHES "^x64($|,)" OR
       CMAKE_VS_PLATFORM_NAME STREQUAL "x64")
  set(CORE_ARCH amd64)
elseif(CMAKE_SYSTEM_PROCESSOR MATCHES "^(ARM64|arm64|aarch64)$")
  set(CORE_ARCH arm64)
else()
  set(CORE_ARCH amd64)
endif()
execute_process(
  COMMAND "${Python3_EXECUTABLE}" -I -X utf8 "${PLUGIN_ROOT}/tool/core_recipe.py"
    --project-dir "${FLUTTER_APPLICATION_PATH}" --target windows
    --windows-arch "${CORE_ARCH}" --print-artifact-pair
  RESULT_VARIABLE CORE_RESULT OUTPUT_VARIABLE CORE_PAIR ERROR_VARIABLE CORE_ERROR
  ENCODING UTF-8 OUTPUT_STRIP_TRAILING_WHITESPACE)
if(NOT CORE_RESULT EQUAL 0)
  message(FATAL_ERROR "${CORE_ERROR}\nRun dart run sing_box_flutter build --target windows --windows-arch ${CORE_ARCH} from the host app first.")
endif()
# Parse one atomic verification result using CMake 3.14 scalar operations. Do not
# turn paths into CMake lists: a semicolon is a valid character in a host path.
string(REPLACE "\r\n" "\n" CORE_PAIR "${CORE_PAIR}")
string(FIND "${CORE_PAIR}" "\n" CORE_SEPARATOR)
if(NOT CORE_SEPARATOR EQUAL 64)
  message(FATAL_ERROR "Invalid verified Windows core path/digest pair.")
endif()
string(SUBSTRING "${CORE_PAIR}" 0 ${CORE_SEPARATOR} CORE_DIGEST)
math(EXPR CORE_PATH_START "${CORE_SEPARATOR} + 1")
string(SUBSTRING "${CORE_PAIR}" ${CORE_PATH_START} -1 CORE_DIR)
string(FIND "${CORE_DIR}" "\n" CORE_EXTRA_LINE)
if(CORE_DIR STREQUAL "" OR NOT CORE_EXTRA_LINE EQUAL -1 OR
   NOT CORE_DIGEST MATCHES "^[0-9a-f]+$")
  message(FATAL_ERROR "Invalid verified Windows core path/digest pair.")
endif()
string(REPLACE "\\" "/" CORE_DIR "${CORE_DIR}")
# Reconfigure when the host changes its selected bundle; always recheck its bytes
# immediately before building so modified binaries cannot bypass the receipt.
set_property(DIRECTORY APPEND PROPERTY CMAKE_CONFIGURE_DEPENDS
  "${FLUTTER_APPLICATION_PATH}/pubspec.yaml"
  "${FLUTTER_APPLICATION_PATH}/.dart_tool/sing_box_flutter/windows.json")
add_custom_target(sing_box_flutter_verify_core ALL
  COMMAND "${Python3_EXECUTABLE}" -I -X utf8 "${PLUGIN_ROOT}/tool/core_recipe.py"
    --project-dir "${FLUTTER_APPLICATION_PATH}" --target windows
    --windows-arch "${CORE_ARCH}" --installed-artifact "${CORE_DIR}"
    --installed-sha256 "${CORE_DIGEST}"
  COMMENT "Verify selected Windows sing-box core and embedded Wintun license"
  VERBATIM)
add_dependencies(${PLUGIN_NAME} sing_box_flutter_verify_core)

# cmake --install does not run build targets or reconfigure. Register this in
# the plugin directory now, before the host's helper-copy rules, for all components.
execute_process(
  COMMAND "${Python3_EXECUTABLE}" -I -X utf8 "${PLUGIN_ROOT}/tool/windows_install.py" guard-settings
    --output "${CMAKE_CURRENT_BINARY_DIR}/sing_box_flutter-install-guard.json"
    --project-dir "${FLUTTER_APPLICATION_PATH}" --core-dir "${CORE_DIR}"
    --core-digest "${CORE_DIGEST}" --architecture "${CORE_ARCH}"
  RESULT_VARIABLE INSTALL_GUARD_RESULT ERROR_VARIABLE INSTALL_GUARD_ERROR ENCODING UTF-8)
if(NOT INSTALL_GUARD_RESULT EQUAL 0)
  message(FATAL_ERROR "${INSTALL_GUARD_ERROR}")
endif()
configure_file("${CMAKE_CURRENT_SOURCE_DIR}/verify_core_install.cmake.in"
  "${CMAKE_CURRENT_BINARY_DIR}/sing_box_flutter-verify-install.cmake" COPYONLY)
install(SCRIPT "${CMAKE_CURRENT_BINARY_DIR}/sing_box_flutter-verify-install.cmake" ALL_COMPONENTS)

string(REPLACE ";" "\\;" CORE_DIR_LIST "${CORE_DIR}")
set(sing_box_flutter_bundled_libraries
  "${CORE_DIR_LIST}/sing_box_flutter_core.exe"
  PARENT_SCOPE)
# File API installer records retain paths as JSON strings, including semicolons
# anywhere in a destination. Request them for this generation, not a later run.
cmake_file_api(QUERY API_VERSION 1 CODEMODEL 2)
set_property(TARGET ${PLUGIN_NAME} PROPERTY SING_BOX_FLUTTER_VERIFIED_CORE "${CORE_DIR}")
set_property(TARGET ${PLUGIN_NAME} PROPERTY SING_BOX_FLUTTER_INSTALL_PYTHON "${Python3_EXECUTABLE}")
set_property(TARGET ${PLUGIN_NAME} PROPERTY SING_BOX_FLUTTER_INSTALL_RESOLVER "${PLUGIN_ROOT}/tool/windows_install.py")
set_property(TARGET ${PLUGIN_NAME} PROPERTY SING_BOX_FLUTTER_INSTALL_TEMPLATE "${CMAKE_CURRENT_SOURCE_DIR}/install_resources.cmake.in")
set_property(TARGET ${PLUGIN_NAME} PROPERTY SING_BOX_FLUTTER_INSTALL_GUARD "${CMAKE_CURRENT_BINARY_DIR}/sing_box_flutter-install-guard.json")
function(_sing_box_flutter_install_resources)
  get_target_property(RESOURCE_CORE_DIR sing_box_flutter_plugin SING_BOX_FLUTTER_VERIFIED_CORE)
  get_target_property(RESOURCE_PYTHON sing_box_flutter_plugin SING_BOX_FLUTTER_INSTALL_PYTHON)
  get_target_property(RESOURCE_RESOLVER sing_box_flutter_plugin SING_BOX_FLUTTER_INSTALL_RESOLVER)
  get_target_property(RESOURCE_TEMPLATE sing_box_flutter_plugin SING_BOX_FLUTTER_INSTALL_TEMPLATE)
  get_target_property(RESOURCE_GUARD sing_box_flutter_plugin SING_BOX_FLUTTER_INSTALL_GUARD)
  set(RESOURCE_SETTINGS "${CMAKE_CURRENT_BINARY_DIR}/sing_box_flutter-install-input.json")
  execute_process(
    COMMAND "${RESOURCE_PYTHON}" -I -X utf8 "${RESOURCE_RESOLVER}" configure
      --output "${RESOURCE_SETTINGS}" --build-dir "${CMAKE_BINARY_DIR}"
      --core-dir "${RESOURCE_CORE_DIR}" --fallback-prefix "${CMAKE_INSTALL_PREFIX}"
      --guard-file "${RESOURCE_GUARD}" "--host-binary=${BINARY_NAME}"
    RESULT_VARIABLE RESOURCE_RESULT ERROR_VARIABLE RESOURCE_ERROR ENCODING UTF-8)
  if(NOT RESOURCE_RESULT EQUAL 0)
    message(FATAL_ERROR "${RESOURCE_ERROR}")
  endif()
  file(GENERATE OUTPUT "${CMAKE_CURRENT_BINARY_DIR}/sing_box_flutter-install-$<CONFIG>.json"
    INPUT "${RESOURCE_SETTINGS}")
  file(GENERATE OUTPUT "${CMAKE_CURRENT_BINARY_DIR}/sing_box_flutter-install-$<CONFIG>.cmake"
    INPUT "${RESOURCE_TEMPLATE}")
  install(SCRIPT "${CMAKE_CURRENT_BINARY_DIR}/sing_box_flutter-install-$<CONFIG>.cmake" COMPONENT Runtime)
endfunction()
cmake_language(DEFER DIRECTORY "${FLUTTER_HOST_DIRECTORY}" CALL _sing_box_flutter_install_resources)
