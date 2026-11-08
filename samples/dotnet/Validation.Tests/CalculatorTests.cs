using Microsoft.VisualStudio.TestTools.UnitTesting;
using Validation;

namespace Validation.Tests;

[TestClass]
public sealed class CalculatorTests
{
    [TestMethod]
    [DataRow(1, 2, 3)]
    [DataRow(-2, 2, 0)]
    public void AddsNumbers(int left, int right, int expected) =>
        Assert.AreEqual(expected, Calculator.Add(left, right));

    [TestMethod]
    public void OverflowIsRejected()
    {
        Assert.ThrowsExactly<OverflowException>(() => Calculator.Add(int.MaxValue, 1));
    }
}
