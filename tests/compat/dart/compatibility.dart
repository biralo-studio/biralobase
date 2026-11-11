import 'dart:io';
import 'package:pocketbase/pocketbase.dart';

void expect(bool value, String message) { if (!value) throw StateError(message); }

Future<void> main() async {
  final url = Platform.environment['BIRALOBASE_TEST_URL']!;
  final admin = PocketBase(url);
  await admin.collection('_superusers').authWithPassword('admin@example.com', 'compatibility-test-password');
  final user = await admin.collection('users').create(body: {
    'email': 'dart@example.com', 'password': 'dart-sdk-test-password', 'passwordConfirm': 'dart-sdk-test-password',
  });
  final notes = Platform.environment['BIRALOBASE_TEST_MODE'] == 'production' ? await admin.collections.getOne('dart_notes') : await admin.collections.create(body: {
    'name': 'dart_notes', 'type': 'base', 'fields': [{'name': 'title', 'type': 'text', 'required': true}],
    'listRule': "@request.auth.id != ''", 'viewRule': "@request.auth.id != ''", 'createRule': "@request.auth.id != ''",
    'updateRule': "@request.auth.id != ''", 'deleteRule': "@request.auth.id != ''",
  });
  final client = PocketBase(url);
  await client.collection('users').authWithPassword('dart@example.com', 'dart-sdk-test-password');
  expect(client.authStore.record?.id == user.id, 'auth record differs');
  await client.collection('users').authRefresh();
  final seen = <String>[];
  await client.collection(notes.id).subscribe('*', (event) { seen.add(event.action); });
  final record = await client.collection(notes.id).create(body: {'title': 'Dart note'});
  expect((await client.collection(notes.id).getOne(record.id)).getStringValue('title') == 'Dart note', 'record differs');
  await client.collection(notes.id).update(record.id, body: {'title': 'updated'});
  expect((await client.collection(notes.id).getList(filter: "title = 'updated'")).totalItems == 1, 'filter differs');
  final anonymous = PocketBase(url);
  expect((await anonymous.collection(notes.id).getList()).totalItems == 0, 'record rules leaked data');
  await client.collection(notes.id).delete(record.id);
  final deadline = DateTime.now().add(const Duration(seconds: 5));
  while (seen.length < 3 && DateTime.now().isBefore(deadline)) { await Future<void>.delayed(const Duration(milliseconds: 25)); }
  expect(seen.join(',') == 'create,update,delete', 'realtime events differ: $seen');
  await client.collection(notes.id).unsubscribe();
  expect((await client.health.check()).code == 200, 'health check failed');
  print('Official Dart SDK compatibility passed');
  // Close SDK transports after the completed assertions.
  exit(0);
}
