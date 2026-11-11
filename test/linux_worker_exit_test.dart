import 'dart:async';
import 'dart:convert';
import 'dart:io';

import 'package:flutter_test/flutter_test.dart';
import 'package:sing_box_flutter/sing_box_flutter_linux.dart';

String workerStat(String state, String start, {int pid = 42, int threads = 1}) {
  final fields = [state, ...List.filled(18, '0'), start, '0', '0'];
  fields[17] = threads.toString(); // num_threads is field 20.
  return '$pid (worker (with) spaces) ${fields.join(' ')}\n';
}

void main() {
  final pendingEntries = <String, Object>{
    'running': workerStat('R', '100'),
    'uninterruptible with SIGKILL pending': workerStat('D', '100'),
    'zombie leader with a live thread': workerStat('Z', '100', threads: 2),
    'terminal state with invalid thread count':
        workerStat('X', '100', threads: 0),
    'malformed': '',
    'different proc PID': workerStat('R', '100', pid: 43),
    'invalid start time': workerStat('R', 'invalid'),
    'unreadable':
        const FileSystemException('denied', '', OSError('denied', 13)),
  };
  for (final entry in pendingEntries.entries) {
    test('worker exit barrier retains ${entry.key} identity', () async {
      final next = Completer<String?>();
      var reads = 0;
      var exited = false;
      final barrier = linuxWaitForWorkerExit(42, '100', interval: Duration.zero,
          readStat: () {
        if (reads++ != 0) return next.future;
        if (entry.value is FileSystemException) {
          return Future<String?>.error(entry.value);
        }
        return Future.value(entry.value as String);
      }).then((_) => exited = true);
      await Future<void>.delayed(const Duration(milliseconds: 5));
      expect(exited, isFalse);
      next.complete(null); // A later observation confirms actual disappearance.
      await barrier;
      expect(exited, isTrue);
    });
  }

  final exitedEntries = <String, String?>{
    'absent': null,
    'zombie': workerStat('Z', '100'),
    'dead': workerStat('X', '100'),
    'reused PID': workerStat('R', '101'),
  };
  for (final entry in exitedEntries.entries) {
    test('worker exit barrier releases ${entry.key}', () async {
      await linuxWaitForWorkerExit(42, '100', readStat: () async => entry.value)
          .timeout(const Duration(seconds: 1));
    });
  }

  test('worker exit barrier observes a real Linux process', () async {
    final worker = await Process.start('/bin/sh', ['-c', 'read line']);
    addTearDown(() async {
      worker.kill(ProcessSignal.sigkill);
      await worker.exitCode;
    });
    final stat =
        latin1.decode(await File('/proc/${worker.pid}/stat').readAsBytes());
    final fields =
        stat.substring(stat.lastIndexOf(')') + 1).trim().split(RegExp(r'\s+'));
    var exited = false;
    final barrier = linuxWaitForWorkerExit(worker.pid, fields[19],
            interval: const Duration(milliseconds: 5))
        .then((_) => exited = true);
    await Future<void>.delayed(const Duration(milliseconds: 15));
    expect(exited, isFalse);
    worker.kill(ProcessSignal.sigkill);
    await worker.exitCode;
    await barrier.timeout(const Duration(seconds: 1));
    expect(exited, isTrue);
  }, skip: !Platform.isLinux);
}
