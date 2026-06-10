export const metadata = {
  title: "UniKernel Next.js example",
  description: "Default Next.js app running on a unikernel",
};

export default function RootLayout({ children }) {
  return (
    <html lang="en">
      <body>{children}</body>
    </html>
  );
}
