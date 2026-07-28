import Foundation
import Security

/// Stores the device token in the Keychain.
///
/// A device token grants full read and write access to everything in the sync
/// folder, so it does not belong in `UserDefaults`, which is a plain plist any
/// process running as this user can read.
enum Keychain {
    private static let service = "dev.schmidt.HomeSync"
    private static let account = "device-token"

    static func readToken() -> String? {
        let query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: account,
            kSecReturnData as String: true,
            kSecMatchLimit as String: kSecMatchLimitOne,
        ]

        var item: CFTypeRef?
        guard SecItemCopyMatching(query as CFDictionary, &item) == errSecSuccess,
              let data = item as? Data,
              let token = String(data: data, encoding: .utf8)
        else { return nil }

        return token
    }

    @discardableResult
    static func writeToken(_ token: String) -> Bool {
        // Deleting first turns "add or update" into one path instead of two,
        // and avoids the update case silently failing when no item exists yet.
        deleteToken()

        guard !token.isEmpty else { return true }

        let query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: account,
            kSecValueData as String: Data(token.utf8),
            // The engine runs at login, before the user unlocks anything, so
            // the item has to be readable once the device itself is unlocked.
            kSecAttrAccessible as String: kSecAttrAccessibleAfterFirstUnlock,
        ]

        return SecItemAdd(query as CFDictionary, nil) == errSecSuccess
    }

    @discardableResult
    static func deleteToken() -> Bool {
        let query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: account,
        ]

        let status = SecItemDelete(query as CFDictionary)
        return status == errSecSuccess || status == errSecItemNotFound
    }
}
