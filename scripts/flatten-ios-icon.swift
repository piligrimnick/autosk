// flatten-ios-icon.swift — turn a full-bleed (but alpha-cornered) iOS app icon
// export into a fully OPAQUE square PNG that Apple will accept.
//
// WHY
//   App Store validation rejects any iOS large app icon that has an alpha
//   channel / transparency (altool error 90717). Icon Composer's iOS export is
//   a full-bleed squircle whose only transparent pixels are the rounded corners
//   (the same corners iOS masks away on device). This tool flattens that image
//   onto an opaque canvas so it validates, while keeping the look identical on
//   device:
//     1. fill the canvas with a solid colour sampled from the icon's top edge
//        (so corners are never black/white if a hairline ever shows),
//     2. draw the icon scaled slightly past the edges so its own gradient bleeds
//        into the corners,
//     3. draw the crisp icon at exact size on top.
//   The output context has no alpha channel, so the PNG is written as opaque RGB.
//
// USAGE: swift flatten-ios-icon.swift <in.png> <out.png> <size>
import Foundation
import CoreGraphics
import ImageIO
import UniformTypeIdentifiers

let args = CommandLine.arguments
guard args.count == 4, let size = Int(args[3]), size > 0 else {
    FileHandle.standardError.write(Data("usage: flatten-ios-icon.swift <in.png> <out.png> <size>\n".utf8))
    exit(2)
}
let inURL = URL(fileURLWithPath: args[1])
let outURL = URL(fileURLWithPath: args[2])

func die(_ m: String) -> Never {
    FileHandle.standardError.write(Data("flatten-ios-icon: \(m)\n".utf8)); exit(1)
}

guard let src = CGImageSourceCreateWithURL(inURL as CFURL, nil),
      let img = CGImageSourceCreateImageAtIndex(src, 0, nil) else { die("cannot load \(inURL.path)") }

let cs = CGColorSpace(name: CGColorSpace.sRGB)!

// Sample a solid fallback colour from the icon's top-edge midpoint (opaque, on
// the gradient). Render the source once into a tiny RGBA buffer to read it.
func sampleTopEdge() -> (CGFloat, CGFloat, CGFloat) {
    let w = img.width, h = img.height
    guard let g = CGContext(data: nil, width: w, height: h, bitsPerComponent: 8,
                            bytesPerRow: w * 4, space: cs,
                            bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue) else { return (0, 0.45, 1) }
    g.draw(img, in: CGRect(x: 0, y: 0, width: w, height: h))
    guard let data = g.data else { return (0, 0.45, 1) }
    let p = data.bindMemory(to: UInt8.self, capacity: w * h * 4)
    // CoreGraphics origin is bottom-left; the icon's top edge is the last row.
    let y = h - 3, x = w / 2
    let o = (y * w + x) * 4
    let a = CGFloat(p[o + 3]) / 255
    if a < 0.5 { return (0, 0.45, 1) }
    return (CGFloat(p[o]) / 255 / a, CGFloat(p[o + 1]) / 255 / a, CGFloat(p[o + 2]) / 255 / a)
}
let (r, gC, b) = sampleTopEdge()

guard let ctx = CGContext(data: nil, width: size, height: size, bitsPerComponent: 8,
                          bytesPerRow: 0, space: cs,
                          bitmapInfo: CGImageAlphaInfo.noneSkipLast.rawValue) else { die("cannot create context") }
ctx.interpolationQuality = .high
let s = CGFloat(size)
ctx.setFillColor(red: r, green: gC, blue: b, alpha: 1)
ctx.fill(CGRect(x: 0, y: 0, width: s, height: s))
let bleed = s * 0.06
ctx.draw(img, in: CGRect(x: -bleed, y: -bleed, width: s + 2 * bleed, height: s + 2 * bleed))
ctx.draw(img, in: CGRect(x: 0, y: 0, width: s, height: s))

guard let out = ctx.makeImage(),
      let dest = CGImageDestinationCreateWithURL(outURL as CFURL, UTType.png.identifier as CFString, 1, nil)
else { die("cannot render output") }
CGImageDestinationAddImage(dest, out, nil)
guard CGImageDestinationFinalize(dest) else { die("cannot write \(outURL.path)") }
