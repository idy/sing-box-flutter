cmake_minimum_required(VERSION 3.19)
project(sing_box_flutter LANGUAGES CXX)

set(PLUGIN_NAME "sing_box_flutter_plugin")
add_library(${PLUGIN_NAME} SHARED
  "sing_box_flutter_plugin_c_api.cpp"
  "include/sing_box_flutter/sing_box_flutter_plugin_c_api.h"
)
apply_standard_settings(${PLUGIN_NAME})
set_target_properties(${PLUGIN_NAME} PROPERTIES CXX_VISIBILITY_PRESET hidden)
target_compile_definitions(${PLUGIN_NAME} PRIVATE FLUTTER_PLUGIN_IMPL)
target_include_directories(${PLUGIN_NAME} INTERFACE
  "${CMAKE_CURRENT_SOURCE_DIR}/include")
target_link_libraries(${PLUGIN_NAME} PRIVATE flutter flutter_wrapper_plugin)

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

string(REPLACE ";" "\\;" CORE_DIR_LIST "${CORE_DIR}")
set(sing_box_flutter_bundled_libraries
  "${CORE_DIR_LIST}/sing_box_flutter_core.exe"
  PARENT_SCOPE)
# Preserve the verified source independently of child-directory variables, then
# register install rules after the host has selected its final install prefix.
set_property(TARGET ${PLUGIN_NAME} PROPERTY SING_BOX_FLUTTER_VERIFIED_CORE "${CORE_DIR}")
function(_sing_box_flutter_install_resources)
  get_target_property(RESOURCE_CORE_DIR sing_box_flutter_plugin SING_BOX_FLUTTER_VERIFIED_CORE)
  get_filename_component(RESOURCE_CORE_PARENT "${RESOURCE_CORE_DIR}" DIRECTORY)
  # Host rules have installed the helper before these deferred resource rules.
  # Follow its actual install-manifest destination, whether the host uses
  # absolute paths, relative paths or an install-time --prefix override.
  file(GENERATE
    OUTPUT "${CMAKE_CURRENT_BINARY_DIR}/sing_box_flutter-prefix-$<CONFIG>.txt"
    CONTENT "${CMAKE_INSTALL_PREFIX}")
  install(CODE [=[
    set(_sbf_original_prefix "${CMAKE_INSTALL_PREFIX}")
    file(READ "${CMAKE_CURRENT_LIST_DIR}/sing_box_flutter-prefix-$<CONFIG>.txt" _sbf_configured_prefix)
    # CMake's manifest is a list. Protect semicolons belonging to either known
    # prefix before iterating its file records.
    string(REPLACE ";" "\\;" _sbf_escaped_prefix "${CMAKE_INSTALL_PREFIX}")
    string(REPLACE "${CMAKE_INSTALL_PREFIX}" "${_sbf_escaped_prefix}" _sbf_manifest "${CMAKE_INSTALL_MANIFEST_FILES}")
    string(REPLACE ";" "\\;" _sbf_escaped_prefix "${_sbf_configured_prefix}")
    string(REPLACE "${_sbf_configured_prefix}" "${_sbf_escaped_prefix}" _sbf_manifest "${_sbf_manifest}")
    set(_sbf_helper_installed FALSE)
    foreach(_sbf_file IN LISTS _sbf_manifest)
      get_filename_component(_sbf_name "${_sbf_file}" NAME)
      if(_sbf_name STREQUAL "sing_box_flutter_core.exe")
        get_filename_component(CMAKE_INSTALL_PREFIX "${_sbf_file}" DIRECTORY)
        set(_sbf_helper_installed TRUE)
      endif()
    endforeach()
    if(NOT _sbf_helper_installed)
      # A resource-only install keeps normal runtime relocation. Only Flutter's
      # unresolved generator-expression prefix needs its generated fallback.
      string(CONCAT _sbf_genex "$" "<")
      string(FIND "${CMAKE_INSTALL_PREFIX}" "${_sbf_genex}" _sbf_expression)
      if(NOT _sbf_expression EQUAL -1)
        set(CMAKE_INSTALL_PREFIX "${_sbf_configured_prefix}")
      endif()
    endif()
  ]=] COMPONENT Runtime)
  install(FILES "${RESOURCE_CORE_PARENT}/core-capabilities.json"
    "${RESOURCE_CORE_PARENT}/native-build.json"
    DESTINATION "sing_box_flutter" COMPONENT Runtime)
  install(DIRECTORY "${RESOURCE_CORE_PARENT}/Legal/"
    DESTINATION "sing_box_flutter/Legal" COMPONENT Runtime
    PATTERN "LICENSE.wintun" EXCLUDE)
  # Install the exact license bytes covered by the receipt and build guard.
  install(FILES "${RESOURCE_CORE_DIR}/LICENSE.wintun"
    DESTINATION "sing_box_flutter/Legal" COMPONENT Runtime)
  install(CODE [=[
    set(CMAKE_INSTALL_PREFIX "${_sbf_original_prefix}")
    unset(_sbf_original_prefix)
    unset(_sbf_helper_installed)
    unset(_sbf_configured_prefix)
    unset(_sbf_escaped_prefix)
    unset(_sbf_manifest)
    unset(_sbf_file)
    unset(_sbf_name)
    unset(_sbf_genex)
    unset(_sbf_expression)
  ]=] COMPONENT Runtime)
endfunction()
cmake_language(DEFER DIRECTORY "${FLUTTER_HOST_DIRECTORY}" CALL _sing_box_flutter_install_resources)
