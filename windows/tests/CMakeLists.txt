cmake_minimum_required(VERSION 3.14)
project(sing_box_flutter_launch_tests LANGUAGES CXX)
add_executable(secure_core_launch_test
  "../secure_core_launch.cpp"
  "../secure_core_launch_test.cpp")
target_compile_features(secure_core_launch_test PRIVATE cxx_std_17)
target_compile_definitions(secure_core_launch_test PRIVATE UNICODE _UNICODE NOMINMAX)
target_include_directories(secure_core_launch_test PRIVATE "../include")
target_link_libraries(secure_core_launch_test PRIVATE advapi32)
if(MSVC)
  target_compile_options(secure_core_launch_test PRIVATE /W4 /WX /EHsc)
endif()
