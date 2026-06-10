// swift-tools-version:5.9
import PackageDescription

// FacetKit — the Facet Architecture client runtime for Apple platforms. It renders
// a server-authored neutral view tree to native SwiftUI views, holds the SSE
// connection, applies live updates by facet id, and forwards taps to the FA event
// endpoint. No application logic lives here — the server is the single source of
// truth, exactly as on web.
let package = Package(
    name: "FacetKit",
    platforms: [.iOS(.v16), .macOS(.v13)],
    products: [
        .library(name: "FacetKit", targets: ["FacetKit"])
    ],
    targets: [
        .target(name: "FacetKit"),
        .testTarget(name: "FacetKitTests", dependencies: ["FacetKit"])
    ]
)
