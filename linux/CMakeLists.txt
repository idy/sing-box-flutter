cmake_minimum_required(VERSION 3.19)
project(sing_box_flutter LANGUAGES NONE)

# ffiPlugin includes this file; Dart registers the runtime implementation.
# Flutter's bundled_libraries install flattens files into lib/ and drops the
# executable mode. Install the private helper directory explicitly instead.
set(sing_box_flutter_bundled_libraries "" PARENT_SCOPE)
function(sing_box_flutter_setup_linux)
  find_package(Python3 3.11 REQUIRED COMPONENTS Interpreter)
  get_filename_component(_sbf_plugin_root "${CMAKE_CURRENT_FUNCTION_LIST_DIR}/.." REALPATH)
  get_filename_component(_sbf_host "${CMAKE_SOURCE_DIR}/.." REALPATH)
  set(_sbf_recipe "${_sbf_plugin_root}/tool/core_recipe.py")

  if(FLUTTER_TARGET_PLATFORM STREQUAL "linux-x64")
    set(_sbf_arch amd64)
  elseif(FLUTTER_TARGET_PLATFORM STREQUAL "linux-arm64")
    set(_sbf_arch arm64)
  elseif(NOT FLUTTER_TARGET_PLATFORM AND CMAKE_SYSTEM_PROCESSOR MATCHES "^(x86_64|amd64|AMD64)$")
    set(_sbf_arch amd64)
  elseif(NOT FLUTTER_TARGET_PLATFORM AND CMAKE_SYSTEM_PROCESSOR MATCHES "^(aarch64|arm64)$")
    set(_sbf_arch arm64)
  else()
    message(FATAL_ERROR "sing_box_flutter supports Linux amd64 and arm64 only.")
  endif()

  set(_sbf_check "${Python3_EXECUTABLE}" -I "${_sbf_recipe}"
    --project-dir "${_sbf_host}" --target linux --arch "${_sbf_arch}")
  execute_process(COMMAND ${_sbf_check} --print-artifact-info
    RESULT_VARIABLE _sbf_result OUTPUT_VARIABLE _sbf_info
    ERROR_VARIABLE _sbf_error OUTPUT_STRIP_TRAILING_WHITESPACE)
  if(NOT _sbf_result EQUAL 0)
    message(FATAL_ERROR
      "sing_box_flutter requires a selected Linux source build for this host.\n"
      "Run: dart run sing_box_flutter build --target linux --arch ${_sbf_arch}\n${_sbf_error}")
  endif()
  string(JSON _sbf_artifact GET "${_sbf_info}" path)
  string(JSON _sbf_capabilities GET "${_sbf_info}" capabilities)
  string(JSON _sbf_digest GET "${_sbf_info}" digest)
  list(APPEND _sbf_check --installed-artifact "${_sbf_artifact}" --installed-sha256 "${_sbf_digest}")

  # A normal Flutter build validates only: it never downloads or compiles Go.
  add_custom_target(sing_box_flutter_validate_core ALL
    COMMAND ${_sbf_check} VERBATIM)
  if(TARGET flutter_assemble)
    add_dependencies(flutter_assemble sing_box_flutter_validate_core)
  endif()
  if(DEFINED BINARY_NAME AND TARGET "${BINARY_NAME}")
    add_dependencies("${BINARY_NAME}" sing_box_flutter_validate_core)
  endif()
  set_property(DIRECTORY APPEND PROPERTY CMAKE_CONFIGURE_DEPENDS
    "${_sbf_host}/pubspec.yaml" "${_sbf_host}/.dart_tool/sing_box_flutter/native.json")

  # Recheck even for `cmake --install` without a preceding build. Bracket-quote
  # each argument so project paths are literal in the generated install script.
  set(_guard "execute_process(COMMAND")
  foreach(_argument IN LISTS _sbf_check)
    set(_equals "=")
    while("${_argument}" MATCHES "]${_equals}]")
      string(APPEND _equals "=")
    endwhile()
    string(APPEND _guard " [${_equals}[${_argument}]${_equals}]")
  endforeach()
  string(APPEND _guard " RESULT_VARIABLE _result)\nif(NOT _result EQUAL 0)\n"
    "  message(FATAL_ERROR \"Selected Linux core validation failed; rebuild the host core.\")\nendif()")
  install(CODE "${_guard}" COMPONENT Runtime)
  install(PROGRAMS "${_sbf_host}/${_sbf_artifact}" DESTINATION "lib/sing_box_flutter" COMPONENT Runtime)
  install(FILES "${_sbf_host}/${_sbf_capabilities}" DESTINATION "lib/sing_box_flutter" COMPONENT Runtime)
endfunction()
# Flutter chooses its bundle prefix and registers cleanup after including plugins.
# Defer all fallible setup until then, and append installs at the root so they
# follow cleanup under both old and new CMP0082 ordering.
cmake_language(DEFER DIRECTORY "${CMAKE_SOURCE_DIR}" CALL sing_box_flutter_setup_linux)
