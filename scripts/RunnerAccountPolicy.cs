using System;
using System.ComponentModel;
using System.Runtime.InteropServices;
using System.Security.Principal;

namespace CLCRunners
{
    // Add one account right without exporting/replacing the machine's security policy.
    // https://learn.microsoft.com/windows/win32/api/ntsecapi/nf-ntsecapi-lsaaddaccountrights
    public static class AccountPolicy
    {
        [StructLayout(LayoutKind.Sequential)]
        private struct ObjectAttributes
        {
            public uint Length;
            public IntPtr RootDirectory, ObjectName;
            public uint Attributes;
            public IntPtr SecurityDescriptor, SecurityQualityOfService;
        }

        [StructLayout(LayoutKind.Sequential)]
        private struct UnicodeString
        {
            public ushort Length, MaximumLength;
            public IntPtr Buffer;
        }

        [DllImport("advapi32.dll")]
        private static extern uint LsaOpenPolicy(IntPtr system, ref ObjectAttributes attributes, uint access, out IntPtr policy);
        [DllImport("advapi32.dll")]
        private static extern uint LsaEnumerateAccountRights(IntPtr policy, byte[] sid, out IntPtr rights, out uint count);
        [DllImport("advapi32.dll")]
        private static extern uint LsaAddAccountRights(IntPtr policy, byte[] sid, UnicodeString[] rights, uint count);
        [DllImport("advapi32.dll")]
        private static extern uint LsaNtStatusToWinError(uint status);
        [DllImport("advapi32.dll")]
        private static extern uint LsaFreeMemory(IntPtr buffer);
        [DllImport("advapi32.dll")]
        private static extern uint LsaClose(IntPtr policy);

        private static void Check(uint status)
        {
            if (status != 0) throw new Win32Exception((int)LsaNtStatusToWinError(status));
        }

        private static byte[] SidBytes(string value)
        {
            var sid = new SecurityIdentifier(value);
            var bytes = new byte[sid.BinaryLength];
            sid.GetBinaryForm(bytes, 0);
            return bytes;
        }

        private static IntPtr Open(uint access)
        {
            var attributes = new ObjectAttributes { Length = (uint)Marshal.SizeOf<ObjectAttributes>() };
            IntPtr policy;
            Check(LsaOpenPolicy(IntPtr.Zero, ref attributes, access, out policy));
            return policy;
        }

        public static string[] GetRights(string sid)
        {
            var policy = Open(0x800); // POLICY_LOOKUP_NAMES
            IntPtr buffer = IntPtr.Zero;
            try
            {
                uint count;
                uint status = LsaEnumerateAccountRights(policy, SidBytes(sid), out buffer, out count);
                if (status == 0xC0000034) return Array.Empty<string>(); // No policy entry for this SID.
                Check(status);
                var result = new string[count];
                int size = Marshal.SizeOf<UnicodeString>();
                for (int i = 0; i < result.Length; i++)
                {
                    var right = Marshal.PtrToStructure<UnicodeString>(IntPtr.Add(buffer, i * size));
                    result[i] = Marshal.PtrToStringUni(right.Buffer, right.Length / 2);
                }
                return result;
            }
            finally
            {
                if (buffer != IntPtr.Zero) LsaFreeMemory(buffer);
                LsaClose(policy);
            }
        }

        public static void GrantBatchLogon(string sid)
        {
            const string name = "SeBatchLogonRight";
            var policy = Open(0x810); // POLICY_LOOKUP_NAMES | POLICY_CREATE_ACCOUNT
            IntPtr buffer = IntPtr.Zero;
            try
            {
                buffer = Marshal.StringToHGlobalUni(name);
                var right = new UnicodeString {
                    Length = (ushort)(name.Length * 2), MaximumLength = (ushort)((name.Length + 1) * 2), Buffer = buffer
                };
                Check(LsaAddAccountRights(policy, SidBytes(sid), new[] { right }, 1));
            }
            finally
            {
                if (buffer != IntPtr.Zero) Marshal.FreeHGlobal(buffer);
                LsaClose(policy);
            }
        }
    }
}
